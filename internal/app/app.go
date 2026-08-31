// Package app wires the live mcpd domain without owning listeners or signals.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/ahodges22/mcpd/internal/stateowner"
	"github.com/ahodges22/mcpd/internal/web"
)

const (
	indexBudget   = 10 * time.Minute
	abstainCosine = 0.2091
	abstainModel  = "text-embedding-3-large"
)

type Paths struct{ Config, State string }

type Options struct {
	Paths            Paths
	OAuthCallbackURL string
	Owner            string
	Logger           *slog.Logger
}

type searchIndex interface {
	mcpsrv.SearchIndex
	QueueRefresh([]catalog.Entry, time.Duration)
	Unvectorized() int
	Model() string
}

// App owns every mutable mcpd worker and store in one state directory.
type App struct {
	cancel    context.CancelFunc
	lease     *stateowner.Lease
	reg       *backend.Registry
	cat       *catalog.Catalog
	index     searchIndex
	liveIndex *searchindex.Live
	secrets   *secretstore.ResolutionCoordinator
	writer    *config.Writer
	manager   *manage.Manager
	state     string
	surface   *web.Server
	remote    *web.Remote
	admin     http.Handler
	protocol  http.Handler
	panel     http.Handler

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func New(parent context.Context, opts Options) (_ *App, err error) {
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if opts.Paths.Config == "" || opts.Paths.State == "" {
		return nil, errors.New("config and state paths are required")
	}
	if opts.OAuthCallbackURL == "" {
		return nil, errors.New("oauth callback URL is required")
	}
	if err := validateCallbackURL(opts.OAuthCallbackURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Owner) == "" {
		return nil, errors.New("owner label is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lease, err := stateowner.Acquire(opts.Paths.State, opts.Owner)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	a := &App{cancel: cancel, lease: lease, shutdownDone: make(chan struct{})}
	constructed := false
	defer func() {
		if !constructed {
			cancel()
			if a.index != nil {
				stopIndex(a.index)
			}
			if a.reg != nil {
				a.reg.Shutdown()
			}
			_ = lease.Close()
		}
	}()

	writer, cfg, err := config.NewWriter(opts.Paths.Config)
	if err != nil {
		return nil, err
	}
	a.writer, a.state = writer, opts.Paths.State
	overrides, err := backend.LoadOverrides(filepath.Join(opts.Paths.State, "overrides.json"), writer)
	if err != nil {
		return nil, err
	}
	store := oauthstore.New(opts.Paths.State, opts.OAuthCallbackURL, writer, oauthstore.Hooks{
		NeedsAuth: func(name, note string) {
			if b, ok := a.reg.Get(name); ok {
				b.NoteNeedsAuth(note)
			}
		},
		Authorized: func(name string) {
			if b, ok := a.reg.Get(name); ok {
				b.NoteAuthorized()
			}
		},
	})
	var resolve func(context.Context, config.SecretConsumer) (map[string]string, error)
	var retry func()
	var secretAuth *secretstore.ControlAuthenticator
	if cfg.Secrets != nil {
		secretAuth, err = secretstore.EnsureControlAuthenticator(opts.Paths.State)
		if err != nil {
			logger.Warn("initialize secret control key", "error", err)
			secretAuth = nil
		}
		provider, openErr := openSecretProvider(opts.Paths.State, cfg.Secrets)
		if openErr != nil {
			provider = secretstore.NewFailedProvider(openErr)
		}
		if cfg.Embeddings.Enabled() {
			a.liveIndex = searchindex.NewLive(opts.Paths.State, cfg.Embeddings, cfg.Ranking)
			a.index = a.liveIndex
		}
		a.secrets = secretstore.NewResolutionCoordinator(cfg, provider, os.LookupEnv, secretstore.ResolutionTuning{}, func(resolved secretstore.ResolvedConsumer) {
			switch resolved.Consumer.Kind {
			case config.ConsumerBackend:
				a.reg.ApplySecretResolution(resolved.Consumer, resolved.Values)
			case config.ConsumerEmbeddings:
				if a.liveIndex == nil {
					return
				}
				if loadErr := a.liveIndex.ApplyAPIKey(resolved.Values[cfg.Embeddings.APIKeyEnv]); loadErr != nil {
					logger.Warn("load search index", "error", loadErr)
				}
				if a.cat != nil {
					a.liveIndex.QueueRefresh(a.cat.Entries(), indexBudget)
				}
			}
		})
		resolve, retry = a.secrets.ResolveConsumer, a.secrets.Retry
	}
	a.reg = backend.NewRegistry(cfg, overrides, backend.Hooks{
		ToolListChanged: func(s string) { a.cat.Trigger(s) }, Reconnected: func(s string) { a.cat.Trigger(s) },
		StopRefresh: func(s string) { a.cat.StopRefresh(s) }, DropTools: func(s string) { a.cat.Drop(s) }, Refresh: func(s string) { a.cat.Trigger(s) },
		AuthHandler: store.Handler, ResolveSecrets: resolve, RetrySecrets: retry,
	})
	a.cat = catalog.New(a.reg, filepath.Join(opts.Paths.State, "catalog.json"))
	if a.secrets != nil {
		a.secrets.SetMutationHooks(secretstore.MutationHooks{
			Reset: func(c config.SecretConsumer) bool {
				if c.Kind == config.ConsumerBackend {
					return a.reg.ResetSecretConsumer(c)
				}
				return c.Kind == config.ConsumerEmbeddings && a.liveIndex != nil
			},
			Pending: func(p secretstore.PendingConsumer) { a.reg.MarkSecretPending(p.Consumer, string(p.Condition)) },
		})
	}
	mgr := manage.New(writer, a.reg, a.cat, overrides, store)
	a.manager = mgr
	if a.secrets != nil {
		mgr.WithSecretIndex(a.secrets)
	}
	if err := mgr.Reconcile(cfg); err != nil {
		return nil, err
	}
	if err := a.cat.Load(); err != nil {
		return nil, err
	}
	for _, name := range a.reg.Names() {
		if a.reg.Health()[name].State == backend.StateDisabled {
			a.cat.Drop(name)
		}
	}
	if cfg.Embeddings.Enabled() && cfg.Secrets == nil {
		idx := searchindex.New(opts.Paths.State, cfg.Embeddings, cfg.Ranking)
		a.index = idx
		if loadErr := idx.Load(); loadErr != nil {
			logger.Warn("load search index", "error", loadErr)
		}
	}
	if a.liveIndex != nil && cfg.Embeddings.APIKeyEnv == "" {
		_ = a.liveIndex.ApplyAPIKey("")
	}
	pass := mcpsrv.NewPassthrough(a.cat, a.reg)
	a.cat.OnCommit(func() {
		pass.Sync()
		if a.index != nil {
			a.index.QueueRefresh(a.cat.Entries(), indexBudget)
		}
	})
	thresholds := a.thresholds()
	ops := mcpsrv.NewOperations(a.cat, a.reg, thresholds, a.index)
	surface := web.New(a.reg, a.cat, web.NewGuard(), store).WithManager(mgr).WithSecrets(a.secrets, secretAuth).WithOperations(ops)
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		surface.WithClients(home, opts.Paths.State)
	}
	if a.index != nil {
		surface.WithUnvectorized(a.index.Unvectorized)
		switch index := a.index.(type) {
		case *searchindex.Index:
			surface.WithSearchStatus(index.Status)
		case *searchindex.Live:
			surface.WithSearchStatus(index.Status)
		}
	}
	a.surface = surface
	a.admin = availability(ctx, surface.AdminHandler())
	protocol := http.NewServeMux()
	protocol.Handle("/search", streamable(mcpsrv.NewSearch(a.cat, a.reg, thresholds, a.index)))
	protocol.Handle("/passthrough", streamable(pass.Server()))
	a.protocol = availability(ctx, protocol)
	a.panel = surface.Handler()
	if a.secrets != nil {
		a.secrets.Start(ctx)
		for _, pending := range a.secrets.Pending() {
			a.reg.MarkSecretPending(pending.Consumer, string(pending.Condition))
		}
	}
	go a.cat.RefreshAll(ctx)
	a.cat.Start(ctx)
	constructed = true
	return a, nil
}

func validateCallbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid oauth callback URL: %w", err)
	}
	if u.Scheme != "http" || u.User != nil || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("oauth callback URL must be an HTTP loopback URL with a path and no user info, query, or fragment")
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return fmt.Errorf("oauth callback URL must include a loopback host and port: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return errors.New("oauth callback URL host must be loopback")
	}
	return nil
}

func availability(ctx context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ctx.Done():
			http.Error(w, "mcpd runtime is shutting down", http.StatusServiceUnavailable)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func streamable(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
}

func (a *App) AdminHandler() http.Handler      { return a.admin }
func (a *App) ProtocolHandler() http.Handler   { return a.protocol }
func (a *App) StandaloneHandler() http.Handler { return a.panel }

// ConfigureStandaloneRemote creates the legacy standalone remote controller.
// It does not bind or start a listener. The command layer owns Apply and Close.
func (a *App) ConfigureStandaloneRemote(addr, hostname string) *web.Remote {
	if a.remote != nil {
		return a.remote
	}
	a.remote = web.NewRemote(a.surface, a.writer, filepath.Join(a.state, "remote-token"), addr, hostname)
	a.surface.WithRemote(a.remote)
	a.manager.ReloadRemote = a.remote.Apply
	a.panel = a.surface.Handler()
	return a.remote
}

func (a *App) Shutdown(ctx context.Context) error {
	a.shutdownOnce.Do(func() { go a.cleanup() })
	select {
	case <-a.shutdownDone:
		return a.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) cleanup() {
	a.cancel()
	if a.remote != nil {
		a.remote.Close()
	}
	if a.secrets != nil {
		a.secrets.Wait()
	}
	if a.index != nil {
		stopIndex(a.index)
	}
	a.cat.WaitIdle()
	a.reg.Shutdown()
	a.shutdownErr = a.lease.Close()
	close(a.shutdownDone)
}

func stopIndex(index searchIndex) {
	switch i := index.(type) {
	case *searchindex.Index:
		i.Stop()
	case *searchindex.Live:
		i.Stop()
	}
}

func (a *App) thresholds() rank.Thresholds {
	if a.index == nil || a.index.Model() != abstainModel {
		return rank.Thresholds{}
	}
	return rank.Thresholds{Cosine: abstainCosine, Enabled: true}
}

func openSecretProvider(state string, declaration *config.Secrets) (secretstore.Provider, error) {
	switch declaration.Provider {
	case config.SecretProviderFile:
		return secretstore.NewFileStore(state)
	case config.SecretProviderNative:
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return secretstore.NewNativeStore(state, executable)
	default:
		return nil, fmt.Errorf("unsupported secret provider %q", declaration.Provider)
	}
}
