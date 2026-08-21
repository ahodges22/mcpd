// Package mcpdcmd is the mcpd command-line interface as an embeddable entrypoint:
// cmd/mcpd calls Main with its process arguments, and a multi-tool binary that
// bundles mcpd calls it the same way. The daemon multiplexes several upstream MCP
// servers behind one loopback endpoint and serves a status surface.
package mcpdcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/manage"
	"github.com/ahodges22/mcpd/internal/mcpsrv"
	"github.com/ahodges22/mcpd/internal/oauthstore"
	"github.com/ahodges22/mcpd/internal/rank"
	"github.com/ahodges22/mcpd/internal/searchindex"
	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/version"
	"github.com/ahodges22/mcpd/internal/web"
)

// shutdownBudget bounds how long exit waits on the registry teardown. Registry.Shutdown
// takes no context and can take none: draining the dispatch gate has no cancellable
// variant, so one tools/call on a backend with no configured timeout would otherwise
// block exit until the client gives up. Bounding it here and exiting regardless is the
// deliberate choice: a systemd restart that hangs is worse than a child that outlives one
// exit, and the child is reaped by the unit's KillMode anyway.
const shutdownBudget = 20 * time.Second

// abstainCosine is the calibrated low-confidence threshold, produced by cmd/evalrank against
// this machine's catalog and checked in rather than configured: the number is only meaningful
// for the embedding model and the tool set it was calibrated over, so it belongs beside the
// code that was measured, and recalibrating means running evalrank and changing it here.
//
// Recalibrated 2026-07-31 for text-embedding-3-large: answerable cosine floor 0.2466, no-answer
// ceiling 0.1716, separated, midpoint 0.2091, 10 of 10 held-out
// no-answer queries correctly flagged. The previous 0.2649 was calibrated over
// text-embedding-3-small and means nothing for these vectors: cosine thresholds do not carry
// across models, which is the whole reason the cache records which model wrote it. The two
// constants move together or not at all: thresholds() disables abstention for any other
// configured model rather than judge its vectors against a number measured for this one.
// Zero disables abstention, which is the right default for a catalog nobody has calibrated
// against.
const abstainCosine = 0.2091
const abstainModel = "text-embedding-3-large"

// indexBudget bounds one detached catalog indexing pass, including cold query expansion.
const indexBudget = 10 * time.Minute

// Main runs the mcpd CLI with the arguments it would receive as os.Args[1:] and
// returns the process exit code. Flag parsing uses a fresh FlagSet per call, so a
// caller that dispatches on its own argv[0] can hand the rest straight through.
func Main(argv []string) int {
	if err := run(argv); err != nil {
		slog.Error("mcpd", "error", err)
		return 1
	}
	return 0
}

func run(argv []string) error {
	// The native-helper check reads the last three arguments, so it gets a full
	// argv shape regardless of what the host binary was invoked as.
	if handled, err := secretstore.ServeNativeHelperIfRequested(context.Background(), append([]string{"mcpd"}, argv...), os.Stdin, os.Stdout); handled {
		return err
	}
	// Serving remains the default. Management subcommands return before daemon flags are parsed.
	if len(argv) > 0 && argv[0] == "install" {
		return runInstall(argv[1:])
	}
	if len(argv) > 0 && argv[0] == "update" {
		return runUpdate(argv[1:])
	}
	if len(argv) > 0 && argv[0] == "secret" {
		return runSecret(argv[1:], defaultSecretCommandDeps())
	}
	if len(argv) > 0 && argv[0] == "service" {
		return runService(argv[1:], defaultServiceCommandDeps())
	}
	if len(argv) > 0 && argv[0] == "doctor" {
		return runDoctor(argv[1:], defaultDoctorCommandDeps())
	}
	if len(argv) > 0 && argv[0] == "setup" {
		return runSetup(argv[1:], defaultSetupCommandDeps())
	}
	fs := flag.NewFlagSet("mcpd", flag.ExitOnError)
	var (
		addr        = fs.String("addr", "127.0.0.1:7420", "loopback address to serve on")
		cfgPath     = fs.String("config", defaultPath("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file")
		statePath   = fs.String("state", defaultPath("XDG_STATE_HOME", ".local/state", ""), "state directory")
		showVersion = fs.Bool("version", false, "print the mcpd version and exit")
	)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	// mcpd has no user authentication, so a non-loopback listener would hand every
	// connected tool to the network. The MCP endpoints' DNS-rebinding defense is also
	// only active on a loopback bind, so binding elsewhere silently drops it. Refuse
	// rather than serve unprotected.
	if err := requireLoopbackAddr(*addr); err != nil {
		return err
	}

	if err := os.MkdirAll(*statePath, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	// Resolved once here so every write and every read uses the same path, and so a
	// configuration symlinked into a dotfiles repository keeps working.
	writer, cfg, err := config.NewWriter(*cfgPath)
	if err != nil {
		return err
	}
	overrides, err := backend.LoadOverrides(filepath.Join(*statePath, "overrides.json"), writer)
	if err != nil {
		return err
	}

	d := &daemon{state: *statePath, writer: writer, overrides: overrides}
	if err := d.wire(cfg, *addr); err != nil {
		return err
	}
	if d.secretCancel != nil {
		defer d.secretCancel()
	}
	return d.serve(*addr)
}

// daemon holds the pieces whose construction is cyclic: the registry's hooks reach the
// catalog, the catalog reads the registry, and the OAuth store's hooks reach the registry.
// Late-bound closures over these fields are what break the cycle.
type daemon struct {
	state        string
	writer       *config.Writer
	overrides    *backend.Overrides
	reg          *backend.Registry
	cat          *catalog.Catalog
	store        *oauthstore.Store
	mgr          *manage.Manager
	pass         *mcpsrv.Passthrough
	index        daemonSearchIndex
	liveIndex    *searchindex.Live
	secrets      *secretstore.ResolutionCoordinator
	secretAuth   *secretstore.ControlAuthenticator
	secretCancel context.CancelFunc
	remote       *web.Remote
	handler      http.Handler
}

type daemonSearchIndex interface {
	mcpsrv.SearchIndex
	QueueRefresh([]catalog.Entry, time.Duration)
	Unvectorized() int
	Model() string
}

func (d *daemon) wire(cfg *config.Config, addr string) error {
	d.store = oauthstore.New(d.state, "http://"+addr+"/oauth/callback", d.writer, oauthstore.Hooks{
		// Nil store hooks fail silently, and needs-auth then never surfaces at all,
		// which is the worse half of getting this wiring wrong.
		NeedsAuth: func(name, note string) {
			if b, ok := d.reg.Get(name); ok {
				b.NoteNeedsAuth(note)
			}
		},
		Authorized: func(name string) {
			if b, ok := d.reg.Get(name); ok {
				b.NoteAuthorized()
			}
		},
	})
	var resolveSecrets func(context.Context, config.SecretConsumer) (map[string]string, error)
	var retrySecrets func()
	if cfg.Secrets != nil {
		authenticator, err := secretstore.EnsureControlAuthenticator(d.state)
		if err != nil {
			slog.Warn("initialize secret control key", "error", err)
		} else {
			d.secretAuth = authenticator
		}
		provider := configuredSecretProvider(d.state, cfg.Secrets)
		if cfg.Embeddings.Enabled() {
			d.liveIndex = searchindex.NewLive(d.state, cfg.Embeddings, cfg.Ranking)
			d.index = d.liveIndex
		}
		d.secrets = secretstore.NewResolutionCoordinator(cfg, provider, os.LookupEnv, secretstore.ResolutionTuning{}, func(resolved secretstore.ResolvedConsumer) {
			switch resolved.Consumer.Kind {
			case config.ConsumerBackend:
				d.reg.ApplySecretResolution(resolved.Consumer, resolved.Values)
			case config.ConsumerEmbeddings:
				if d.liveIndex == nil {
					return
				}
				if err := d.liveIndex.ApplyAPIKey(resolved.Values[cfg.Embeddings.APIKeyEnv]); err != nil {
					slog.Warn("load search index", "error", err)
				}
				if d.cat != nil {
					if entries := d.cat.Entries(); len(entries) > 0 {
						d.liveIndex.QueueRefresh(entries, indexBudget)
					}
				}
			}
		})
		resolveSecrets = d.secrets.ResolveConsumer
		retrySecrets = d.secrets.Retry
	}
	d.reg = backend.NewRegistry(cfg, d.overrides, backend.Hooks{
		ToolListChanged: func(s string) { d.cat.Trigger(s) },
		Reconnected:     func(s string) { d.cat.Trigger(s) },
		StopRefresh:     func(s string) { d.cat.StopRefresh(s) },
		DropTools:       func(s string) { d.cat.Drop(s) },
		Refresh:         func(s string) { d.cat.Trigger(s) },
		// A nil AuthHandler fails loudly at the first dial of an OAuth backend.
		AuthHandler:    d.store.Handler,
		ResolveSecrets: resolveSecrets,
		RetrySecrets:   retrySecrets,
	})
	d.cat = catalog.New(d.reg, filepath.Join(d.state, "catalog.json"))
	if d.secrets != nil {
		d.secrets.SetMutationHooks(secretstore.MutationHooks{
			Reset: func(consumer config.SecretConsumer) bool {
				switch consumer.Kind {
				case config.ConsumerBackend:
					return d.reg.ResetSecretConsumer(consumer)
				case config.ConsumerEmbeddings:
					return d.liveIndex != nil
				default:
					return false
				}
			},
			Pending: func(pending secretstore.PendingConsumer) {
				d.reg.MarkSecretPending(pending.Consumer, string(pending.Condition))
			},
		})
	}

	d.mgr = manage.New(d.writer, d.reg, d.cat, d.overrides, d.store)
	if d.secrets != nil {
		d.mgr.WithSecretIndex(d.secrets)
	}

	// The backstop for a crash between a commit and its cleanup, and the only thing that
	// catches a declaration removed or repointed by hand while the daemon was stopped.
	if err := d.mgr.Reconcile(cfg); err != nil {
		return err
	}

	if err := d.cat.Load(); err != nil {
		return err
	}
	// A crash between an override write and its tool eviction leaves a disabled backend's
	// tools in the persisted catalog, and a disabled backend is never re-listed, so this
	// is the only thing that ever removes them.
	for _, name := range d.reg.Names() {
		if d.reg.Health()[name].State == backend.StateDisabled {
			d.cat.Drop(name)
		}
	}

	// Embeddings, when a gateway is configured. Without this every query reaches rank.Fuse
	// with nil vectors, so fusion degrades to lexical only and abstention is inert: Tasks 6
	// and 7 are dead code in production until it is wired.
	if cfg.Embeddings.Enabled() && cfg.Secrets == nil {
		// The client first, so the cache records the model actually sent rather than the one
		// configured: an empty configuration resolves to a default, and a header saying "" would
		// be a claim about a model that does not exist.
		index := searchindex.New(d.state, cfg.Embeddings, cfg.Ranking)
		d.index = index
		if err := index.Load(); err != nil {
			// A cold cache is a slower first refresh, not a failure.
			slog.Warn("load search index", "error", err)
		}
	}
	if d.liveIndex != nil && cfg.Embeddings.APIKeyEnv == "" {
		if err := d.liveIndex.ApplyAPIKey(""); err != nil {
			slog.Warn("load search index", "error", err)
		}
	}

	// Built after Load, because the constructor syncs and would otherwise serve an empty
	// tool set until the first refresh commits.
	d.pass = mcpsrv.NewPassthrough(d.cat, d.reg)
	// Post-commit rather than the pre-commit tool-list-changed hook, which would sync
	// against stale entries. Sync takes only its own lock and reaches the catalog solely
	// through Entries, which is what makes it safe on the Drop path: that one runs inside
	// a lifecycle teardown with the backend's dispatch gate held closed, so a hook that
	// waited on a tool call would deadlock the daemon permanently.
	d.cat.OnCommit(func() {
		d.pass.Sync()
		if d.index == nil {
			return
		}
		// Bounded and detached: this hook also fires from Drop inside a lifecycle
		// teardown, which holds that backend's dispatch gate closed until the hook
		// returns, so a gateway call made inline would delay every disable by its own
		// timeout.
		d.index.QueueRefresh(d.cat.Entries(), indexBudget)
	})

	guard := web.NewGuard()
	surface := web.New(d.reg, d.cat, guard, d.store).WithManager(d.mgr).WithSecrets(d.secrets, d.secretAuth)
	hostname, _ := os.Hostname()
	d.remote = web.NewRemote(surface, d.writer, filepath.Join(d.state, "remote-token"), cfg.Remote.Addr, hostname)
	surface = surface.WithRemote(d.remote)
	// A reload that adopts a hand-edited remote declaration must reach the live
	// listener, or the file and the network would describe different states.
	// The listener itself starts in serve, after the main listener binds: a
	// misconfigured remote address that overlaps the main one must lose that
	// race, not win it and take the daemon down.
	d.mgr.ReloadRemote = d.remote.Apply
	mux := http.NewServeMux()
	// Both MCP handlers are wrapped in the same guard value the web surface uses, so the
	// cross-origin policy cannot diverge between the two surfaces.
	var index mcpsrv.SearchIndex
	if d.index != nil {
		index = d.index
		surface = surface.WithUnvectorized(d.index.Unvectorized)
	}
	mux.Handle("/mcp/search", guard.Protect(streamable(mcpsrv.NewSearch(d.cat, d.reg, d.thresholds(), index))))
	mux.Handle("/mcp/passthrough", guard.Protect(streamable(d.pass.Server())))
	mux.Handle("/", surface.Handler())
	d.handler = mux
	if d.secrets != nil {
		secretCtx, cancel := context.WithCancel(context.Background())
		d.secretCancel = cancel
		d.secrets.Start(secretCtx)
		for _, pending := range d.secrets.Pending() {
			d.reg.MarkSecretPending(pending.Consumer, string(pending.Condition))
		}
	}
	return nil
}

func configuredSecretProvider(state string, declaration *config.Secrets) secretstore.Provider {
	provider, err := openSecretProvider(state, declaration)
	if err != nil {
		slog.Warn("initialize secret provider", "provider", declaration.Provider, "error", err)
		return secretstore.NewFailedProvider(err)
	}
	return provider
}

// thresholds is the abstention configuration this daemon runs with. Abstention is only
// meaningful with a gateway: with no vectors there is no cosine to judge against, and
// LowConfidence goes quiet rather than flagging every query.
func (d *daemon) thresholds() rank.Thresholds {
	if d.index == nil || abstainCosine <= 0 {
		return rank.Thresholds{}
	}
	if d.index.Model() != abstainModel {
		slog.Warn("abstention disabled: threshold calibrated for a different embedding model",
			"calibrated", abstainModel, "configured", d.index.Model())
		return rank.Thresholds{}
	}
	return rank.Thresholds{Cosine: abstainCosine, Enabled: true}
}

func streamable(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
}

// requireLoopbackAddr reports an error unless addr binds a loopback host. An empty host
// (a bare ":port") binds every interface and is refused too.
func requireLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -addr %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("-addr %q binds every interface; mcpd serves loopback only", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-addr %q is not a loopback address; mcpd serves loopback only", addr)
}

func (d *daemon) serve(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: d.handler}
	// Only after the main listener holds its port: a remote declaration that
	// names an overlapping address then fails its own bind and stays off,
	// which is the harmless half of that mistake.
	d.remote.Apply()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	defer cancelRefresh()
	// Start covers the TTL tick only and deliberately performs no immediate refresh, so a
	// cold start needs this one explicitly rather than doubling every start's reads. It
	// runs detached, and that is not a detail: a first refresh dials every backend and
	// vectorizes the whole catalog, so serving only afterwards leaves the listener bound
	// and silent for as long as that takes, which reads as a hung daemon.
	go d.cat.RefreshAll(refreshCtx)
	d.cat.Start(refreshCtx)

	errs := make(chan error, 1)
	go func() {
		slog.Info("mcpd serving", "addr", addr, "version", version.String(), "backends", len(d.reg.Names()))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	slog.Info("mcpd shutting down")
	shutCtx, cancelShut := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancelShut()
	d.remote.Close()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Warn("http shutdown", "error", err)
	}
	// Cancelled before the teardown even though the teardown makes every dial refuse, so
	// no refresh loop keeps running against a registry that refuses it.
	cancelRefresh()

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.reg.Shutdown()
	}()
	select {
	case <-done:
	case <-shutCtx.Done():
		slog.Warn("registry teardown exceeded its budget; exiting anyway", "budget", shutdownBudget)
	}
	return nil
}

// defaultPath resolves an XDG location under a per-user mcpd directory, falling back to
// the conventional relative path when the variable is unset.
func defaultPath(env, fallback, file string) string {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, fallback)
	}
	dir := filepath.Join(base, "mcpd")
	if file == "" {
		return dir
	}
	return filepath.Join(dir, file)
}
