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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/version"
	"github.com/ahodges22/mcpd/internal/web"
	mcpdruntime "github.com/ahodges22/mcpd/runtime"
)

// shutdownBudget bounds how long exit waits on the registry teardown. Registry.Shutdown
// takes no context and can take none: draining the dispatch gate has no cancellable
// variant, so one tools/call on a backend with no configured timeout would otherwise
// block exit until the client gives up. Bounding it here and exiting regardless is the
// deliberate choice: a systemd restart that hangs is worse than a child that outlives one
// exit, and the child is reaped by the unit's KillMode anyway.
const shutdownBudget = 20 * time.Second

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
	if len(argv) > 0 && argv[0] == "tray" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		return runTray(argv[1:], defaultTrayCommandDeps())
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

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rt, err := mcpdruntime.New(signalCtx, mcpdruntime.Options{
		Paths:            mcpdruntime.Paths{Config: *cfgPath, State: *statePath},
		OAuthCallbackURL: "http://" + *addr + "/oauth/callback",
		Owner:            "standalone mcpd",
	})
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	remote := rt.StandaloneRemote("", hostname)
	return serveRuntime(signalCtx, *addr, rt, remote)
}

func serveRuntime(ctx context.Context, addr string, rt *mcpdruntime.Runtime, remote mcpdruntime.StandaloneRemote) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		_ = rt.Shutdown(shutCtx)
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	guard := web.NewGuard()
	mux := http.NewServeMux()
	mux.Handle("/mcp/", guard.Protect(http.StripPrefix("/mcp", rt.ProtocolHandler())))
	mux.Handle("/", rt.StandaloneHandler())
	srv := &http.Server{Handler: mux}
	remote.Apply()
	errs := make(chan error, 1)
	go func() {
		slog.Info("mcpd serving", "addr", addr, "version", mcpdruntime.Version)
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()
	select {
	case err := <-errs:
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		_ = rt.Shutdown(shutCtx)
		return err
	case <-ctx.Done():
	}
	slog.Info("mcpd shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Warn("http shutdown", "error", err)
	}
	if err := rt.Shutdown(shutCtx); err != nil {
		slog.Warn("runtime shutdown", "error", err)
	}
	return nil
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
