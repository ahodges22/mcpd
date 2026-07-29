package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/testfake"
)

const (
	// childPIDFile names the file a helper child appends its pid to.
	childPIDFile = "MCPD_TEST_CHILD_PIDFILE"
	// childMarker turns this test binary into a stdio backend. It has to be argv[1]
	// rather than an environment variable alone, because an exported variable would
	// otherwise make this whole package exit zero having run no test at all.
	childMarker = "mcpd-test-stdio-child"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == childMarker {
		serveAsChild(os.Getenv(childPIDFile))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serveAsChild runs this binary as a real stdio MCP backend, so a disable can be
// asserted against a process that genuinely exists.
func serveAsChild(pidFile string) {
	if pidFile == "" {
		panic("child mode without " + childPIDFile)
	}
	f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	testfake.New("child", tool("kubectl_logs")).Server().Run(context.Background(), &mcp.StdioTransport{})
}

func TestDisableRemovesToolsAndTerminatesTheChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	reg, c := lifecycle(t, dir, fastTuning, config.Backend{
		Name:       "child",
		Command:    self,
		Args:       []string{childMarker},
		Env:        map[string]string{childPIDFile: pidFile},
		TimeoutSec: 30,
	})

	c.RefreshAll(t.Context())
	if _, ok := c.Lookup("mcp__child__kubectl_logs"); !ok {
		t.Fatalf("the child's tools were never catalogued: %v", c.Errors())
	}
	pids := childPIDs(t, pidFile)
	if len(pids) != 1 {
		t.Fatalf("pids = %v, want exactly one child", pids)
	}

	if err := reg.Disable("child"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, ok := c.Lookup("mcp__child__kubectl_logs"); ok {
		t.Error("a disabled backend's tools are still in the catalog, so both endpoints still advertise them")
	}
	if alive(pids[0]) {
		t.Errorf("child %d is still running after a disable", pids[0])
	}
	if h := reg.Health()["child"]; h.State != backend.StateDisabled || h.ToolCount != 0 {
		t.Errorf("health = %+v, want a disabled backend serving no tools", h)
	}

	// Nothing may bring it back: a trigger after the disable must neither respawn
	// the child nor restore its tools.
	c.Trigger("child")
	c.WaitIdle()
	if got := childPIDs(t, pidFile); len(got) != 1 {
		t.Errorf("pids = %v: a disabled backend was respawned", got)
	}
	if _, ok := c.Lookup("mcp__child__kubectl_logs"); ok {
		t.Error("a refresh after the disable resurrected a disabled backend's tools")
	}
	if got := c.Errors()["child"]; got != "" {
		t.Errorf("recorded error %q: a disabled backend is not a failing one", got)
	}
}

func TestALateRefreshCannotResurrectADisabledBackend(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	gate := holdLists(t, fake, 2)
	gate.release(1) // only the second read is held
	reg, c := lifecycle(t, t.TempDir(), fastTuning, servedOverHTTP(t, fake))

	c.Refresh(t.Context(), "alpha")
	if got := ids(c); len(got) != 1 {
		t.Fatalf("ids = %v, want the backend's tools before it is disabled", got)
	}

	c.Trigger("alpha")
	waitFor(t, func() bool { return fake.ListCalls.Load() == 2 })
	if err := reg.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	gate.release(2) // the superseded read answers only now
	c.WaitIdle()

	if got := ids(c); len(got) != 0 {
		t.Errorf("ids = %v, want none: a tools/list in flight when the backend was disabled resurrected it", got)
	}
	if got := c.Errors()["alpha"]; got != "" {
		t.Errorf("recorded error %q: being disabled is not a backend failure", got)
	}
	// The health record has to agree: the read we cancelled failed because we
	// disabled the backend, and a disabled backend showing an error text reads as a
	// broken one on the status surface.
	if got := reg.Health()["alpha"]; got.State != backend.StateDisabled || got.LastErr != "" || got.ToolCount != 0 {
		t.Errorf("health = %+v, want a disabled backend with no tools and no error", got)
	}
}

func TestAScheduledRetryDoesNotFireAtADisabledBackend(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	tune := fastTuning
	tune.backoffBase, tune.ttl = 2*time.Second, time.Hour
	reg, c := lifecycle(t, t.TempDir(), tune, servedOverHTTP(t, fake))
	// A change reported during the read leaves the loop scheduled to read again
	// after its backoff, which is the retry a disable has to cancel and await.
	fake.BeforeList = func() { c.Trigger("alpha") }

	c.Trigger("alpha")
	waitFor(t, func() bool { return len(c.Entries()) == 1 })
	before := fake.Received()

	start := time.Now()
	if err := reg.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Disable took %s: it waited the retry out instead of cancelling it", elapsed)
	}
	if refreshing(c) {
		t.Error("a refresh is still scheduled after the disable returned, so a retry can still reconnect the backend")
	}
	if got := fake.Received(); !slices.Equal(got, before) {
		t.Errorf("the upstream received %v after the disable: the scheduled retry fired anyway", got[len(before):])
	}
}

func TestDisableDoesNotHangOnAnInFlightHandshake(t *testing.T) {
	// The handshake is where connect holds the lifecycle mutex. A disable that
	// took that mutex without first cancelling the handshake would wait the whole
	// handshake budget out rather than interrupting it.
	handshakes, stop := make(chan struct{}, 4), make(chan struct{})
	var handshake sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parked := false
		handshake.Do(func() { parked = true })
		if !parked {
			// Everything after the handshake is answered, including the SDK's own
			// cancellation notification: unanswered, that notification's internal 5s
			// timeout is what this test would end up measuring.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		handshakes <- struct{}{}
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	// Registered last, so it runs first: a parked handler blocks the server close.
	t.Cleanup(func() { close(stop) })
	reg, c := lifecycle(t, t.TempDir(), fastTuning, config.Backend{Name: "alpha", HTTPURL: srv.URL, TimeoutSec: 30})

	c.Trigger("alpha")
	<-handshakes

	done := make(chan error, 1)
	go func() { done <- reg.Disable("alpha") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Disable did not return while a handshake was in flight: it awaited the handshake rather than cancelling it")
	}

	// The handshake we aborted must not be reported as the backend failing: a
	// backend left in a down state accepts the next dispatch, and reconnects.
	if got := reg.Health()["alpha"].State; got != backend.StateDisabled {
		t.Errorf("state = %q, want %q: the aborted handshake overwrote the disable", got, backend.StateDisabled)
	}
	c.WaitIdle()
	if got := c.Errors()["alpha"]; got != "" {
		t.Errorf("recorded error %q for a handshake our own disable aborted", got)
	}
}

// --- helpers ---

// lifecycle wires a real registry and a real catalog to each other, which is the
// only way to assert what a disable does to the catalog.
func lifecycle(t *testing.T, dir string, tune tuning, specs ...config.Backend) (*backend.Registry, *Catalog) {
	t.Helper()
	cfg := &config.Config{Backends: make(map[string]config.Backend, len(specs))}
	for _, spec := range specs {
		cfg.Backends[spec.Name] = spec
	}
	ov, err := backend.LoadOverrides(filepath.Join(dir, "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}

	var c *Catalog
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{
		ToolListChanged: func(server string) { c.Trigger(server) },
		Reconnected:     func(server string) { c.Trigger(server) },
		StopRefresh:     func(server string) { c.StopRefresh(server) },
		DropTools:       func(server string) { c.Drop(server) },
		Refresh:         func(server string) { c.Trigger(server) },
	})
	c = newCatalog(registrySource{reg}, filepath.Join(dir, "catalog.json"), tune)
	t.Cleanup(func() {
		stopAll(c, reg.Names())
		// Disable is also the only exported way to be sure no session, and no child,
		// outlives the test.
		for _, name := range reg.Names() {
			reg.Disable(name)
		}
	})
	return reg, c
}

// servedOverHTTP publishes a fake on a loopback server, so the registry dials a
// real transport. Stateless keeps the client from holding a standalone SSE
// stream open, which would block the test server's shutdown.
func servedOverHTTP(t *testing.T, f *testfake.Fake) config.Backend {
	t.Helper()
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return f.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		f.Close()
	})
	return config.Backend{Name: f.Name, HTTPURL: srv.URL, TimeoutSec: 10}
}

func refreshing(c *Catalog) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busyLocked()
}

func childPIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("pid file holds %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
