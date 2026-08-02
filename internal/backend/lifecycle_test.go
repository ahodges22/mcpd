package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
)

func TestADisabledOverrideOutlivesARestart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	declared := []byte(`{"backends":{"alpha":{"command":"unused"}}}`)
	if err := os.WriteFile(cfgPath, declared, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	statePath := filepath.Join(dir, "overrides.json")

	r := NewRegistry(cfg, overridesAt(t, statePath), Hooks{})
	if err := r.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// The restart: a fresh registry over the same declarations and the same state.
	restarted := NewRegistry(cfg, overridesAt(t, statePath), Hooks{})
	b, _ := restarted.Get("alpha")
	var dials atomic.Int64
	b.dial = func(context.Context) (mcp.Transport, error) {
		dials.Add(1)
		return nil, errors.New("dialled a disabled backend")
	}

	if got := b.Health().State; got != StateDisabled {
		t.Errorf("state after restart = %q, want %q", got, StateDisabled)
	}
	if _, err := b.Call(t.Context(), "kubectl_logs", nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("call err = %v, want ErrDisabled", err)
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("dials = %d, want 0: a disabled backend must not be connected on startup", n)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(after, declared) {
		t.Errorf("the daemon rewrote the user's configuration file: %s", after)
	}
}

func TestADispatchCannotOutrunADisable(t *testing.T) {
	fake := testfake.New("alpha", tool("open_pull_request"))
	atWrite, release := make(chan struct{}), make(chan struct{})
	var unpark sync.Once
	// The parked write must be released even on a failure: an unreleased one blocks
	// the session close in this test's cleanup.
	releaseWrite := func() { unpark.Do(func() { close(release) }) }
	var writes atomic.Int64
	var atStop, atDrop []string

	r := wire(t, Hooks{
		StopRefresh: func(string) { atStop = fake.Received() },
		DropTools:   func(string) { atDrop = fake.Received() },
	}, fake)
	// Registered after wire, so it runs before wire's session close: a parked write
	// blocks that close.
	t.Cleanup(releaseWrite)
	b, _ := r.Get("alpha")
	inner := b.dial
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		tr, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		// The second tools/call parks between its enabled check and its request
		// existing on the wire, which is the window the gate has to cover.
		return &parkWrites{inner: tr, park: func(method string) {
			if method == "tools/call" && writes.Add(1) == 2 {
				close(atWrite)
				<-release
			}
		}}, nil
	}

	if _, err := b.Call(t.Context(), "open_pull_request", nil); err != nil {
		t.Fatalf("warm-up call: %v", err)
	}

	inflight := make(chan error, 1)
	go func() {
		_, err := b.Call(context.Background(), "open_pull_request", nil)
		inflight <- err
	}()
	<-atWrite

	disabled := make(chan error, 1)
	go func() { disabled <- r.Disable("alpha") }()
	select {
	case err := <-disabled:
		t.Fatalf("Disable returned (err=%v) while a dispatch held a lease: it did not drain the gate", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseWrite()
	assertOutcomeUnknown(t, <-inflight, "open_pull_request")
	if err := <-disabled; err != nil {
		t.Fatalf("disable: %v", err)
	}

	// The caller cannot tell a cancelled send from a completed one; the upstream's
	// request log is what proves cancellation stopped this one before delivery.
	if _, err := b.Call(t.Context(), "open_pull_request", nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("call after the disable err = %v, want ErrDisabled", err)
	}
	final := fake.Received()
	if n := countCalls(final); n != 1 {
		t.Errorf("upstream received %d tools/call requests out of 3 attempts, want 1: %v", n, final)
	}
	if !slices.Equal(atStop, final) {
		t.Errorf("the upstream received %v after the gate closed: it held %v when the teardown began", final[len(atStop):], atStop)
	}
	if !slices.Equal(atDrop, final) {
		t.Errorf("the upstream received %v after the session was closed", final[len(atDrop):])
	}
}

func TestEnableCannotInterleaveWithATeardown(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	enabled := make(chan error, 1)
	var r *Registry
	r = wire(t, Hooks{StopRefresh: func(string) {
		// A re-enable landing mid-teardown must wait for it: allowed through, it
		// would clear the disabled state while the session is still being closed,
		// and the next dispatch would spawn a second child behind the first.
		go func() { enabled <- r.Enable("alpha") }()
		select {
		case err := <-enabled:
			t.Errorf("Enable completed inside a teardown (err=%v): the two are not serialized", err)
		case <-time.After(100 * time.Millisecond):
		}
	}}, fake)
	b, _ := r.Get("alpha")

	if _, err := b.Call(t.Context(), "kubectl_logs", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := r.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	select {
	case err := <-enabled:
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-enable never completed after the teardown finished")
	}

	if got := b.Health().State; got != StateDown {
		t.Errorf("state = %q, want %q: the enable ran after the teardown, so it must have won", got, StateDown)
	}
	if r.overrides.Disabled("alpha") {
		t.Error("the persisted override still says disabled, so a restart would contradict the running daemon")
	}
	b.mu.Lock()
	leaked := b.session != nil
	b.mu.Unlock()
	if leaked {
		t.Error("a session survived the teardown, so the child it owns was never terminated")
	}
}

func TestDisableInterruptsAHandshakeItCannotDrain(t *testing.T) {
	// A tools/list takes no dispatch lease, so draining the gate does not await the
	// handshake one of them started. Nothing but cancelling that handshake ends it,
	// and connect holds the lifecycle mutex throughout, so taking that mutex first
	// waits the whole handshake budget out instead of interrupting it.
	r := NewRegistry(stdioConfig("alpha"), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	dialing := make(chan struct{})
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go b.ListTools(context.Background())
	<-dialing

	done := make(chan error, 1)
	go func() { done <- r.Disable("alpha") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Disable did not return within 5s of a handshake it cannot drain, whose budget is %s", b.ConnectTimeout())
	}
}

func TestDisableInterruptsAHandshakeADispatchIsWaitingOn(t *testing.T) {
	// A tools/call holds its dispatch lease for the whole handshake it triggers, so
	// draining the gate means waiting for that handshake. Cancelling it only once the
	// gate is held would therefore wait for the very handshake the cancellation ends.
	// An OAuth code fetch blocks for a human inside that handshake, which is what turns
	// this from a worst case into the normal case for an unauthorized backend.
	r := NewRegistry(stdioConfig("alpha"), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	dialing := make(chan struct{})
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	call := make(chan error, 1)
	go func() {
		_, err := b.Call(context.Background(), "open_pull_request", nil)
		call <- err
	}()
	<-dialing

	done := make(chan error, 1)
	go func() { done <- r.Disable("alpha") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Disable did not return within 5s while a dispatch awaited a handshake, whose budget is %s", b.ConnectTimeout())
	}
	if err := <-call; !errors.Is(err, ErrNotAttempted) {
		t.Errorf("the interrupted dispatch err = %v, want it to wrap ErrNotAttempted: nothing was sent, so the caller may retry", err)
	}
}

func TestDisableInterruptsAnInflightDispatchBeforeDrainingTheGate(t *testing.T) {
	fake := testfake.New("alpha", tool("open_pull_request"))
	delivered := make(chan struct{})
	fake.OnCall = func(ctx context.Context, _ *mcp.CallToolRequest) {
		close(delivered)
		<-ctx.Done()
	}
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	callCtx, cancelCall := context.WithCancel(context.Background())
	t.Cleanup(cancelCall)

	call := make(chan error, 1)
	go func() {
		_, err := b.Call(callCtx, "open_pull_request", nil)
		call <- err
	}()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the upstream, so this test proves nothing")
	}

	done := make(chan error, 1)
	go func() { done <- r.Disable("alpha") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Disable did not cancel an in-flight dispatch before draining the gate")
	}
	select {
	case err := <-call:
		assertOutcomeUnknown(t, err, "open_pull_request")
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled dispatch did not return after Disable completed")
	}
}

func TestATeardownFlagsItselfBeforeCancellingAHandshake(t *testing.T) {
	// Observe stopping from the first cancellation, before teardown's state write.
	r := NewRegistry(stdioConfig("alpha"), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	var cancels int
	var flaggedFirst bool
	b.mu.Lock()
	b.connectCancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if cancels == 0 {
			flaggedFirst = b.stopping
		}
		cancels++
	}
	b.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- r.Reconnect("alpha") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconnect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect hung while cancelling the in-flight handshake")
	}
	if cancels == 0 {
		t.Fatal("teardown cancelled no handshake, so this proves nothing")
	}
	if !flaggedFirst {
		t.Error("teardown cancelled the in-flight handshake before flagging itself, so a dispatch in its prologue can start a fresh one")
	}
}

func TestNoHandshakeStartsInsideATeardown(t *testing.T) {
	// The window the stopping flag closes: a dispatch that took its lease before the
	// teardown began can reach connect after the teardown's cancellation, and would then
	// park a fresh handshake behind a gate.Lock that is waiting for that dispatch's own
	// lease, with nothing left to interrupt it. A reconnect leaves the state at down, so
	// the flag is the only thing that can refuse that handshake, which is what makes this
	// probe sound. It runs from inside stopRefresh, where the gate is held and the
	// lifecycle mutex is free.
	// No warm-up call: with a session still open, connect hands that session back before
	// it reaches the flag, and the case that matters is the one with a handshake to start.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	inner := b.dial
	var dials atomic.Int64
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		dials.Add(1)
		return inner(ctx)
	}
	var reentered error
	b.stopRefresh = func(string) { _, reentered = b.connect(context.Background()) }

	if err := r.Reconnect("alpha"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if reentered == nil {
		t.Error("a handshake started inside a teardown, so it can park behind the gate that teardown is waiting for")
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("dials = %d, want 0: a handshake started inside a teardown", n)
	}
	// And the flag is lifted afterwards, or the backend could never connect again.
	if _, err := b.ListTools(t.Context()); err != nil {
		t.Errorf("list after the teardown: %v", err)
	}
}

func TestTheRefreshLoopIsStoppedWithTheGateClosedAndTheLifecycleMutexFree(t *testing.T) {
	// Stopping the refresh loop awaits it, and a read inside that loop can be
	// blocked in connect waiting for the lifecycle mutex. Awaiting the loop while
	// holding that mutex is a deadlock rather than a slow path. The gate must
	// already be closed, or a dispatch could still write while the tools go.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	var heldLife, gateOpen bool
	// Assigned after wire, because the probe needs the backend it is asking about.
	b.stopRefresh = func(string) {
		if heldLife = !b.life.TryLock(); !heldLife {
			b.life.Unlock()
		}
		if gateOpen = b.gate.TryRLock(); gateOpen {
			b.gate.RUnlock()
		}
	}

	if _, err := b.Call(t.Context(), "kubectl_logs", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := r.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if heldLife {
		t.Error("the refresh loop is awaited under the lifecycle mutex: a read blocked in connect can never exit, so the disable deadlocks")
	}
	if gateOpen {
		t.Error("the refresh loop is stopped before the dispatch gate closed, so a dispatch could still be in flight")
	}
}

func TestAHandshakeCompletingUnderADisableInstallsNoSession(t *testing.T) {
	// connect holds the lifecycle mutex across a whole handshake, so a disable can
	// be waiting for that mutex while a handshake completes, and a cancellation can
	// arrive too late to stop it. Installing that session would leave the backend
	// reporting down rather than disabled, and the next dispatch would use it.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	inner := b.dial
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		b.mu.Lock()
		b.health.State = StateDisabled
		b.mu.Unlock()
		return inner(ctx)
	}

	sess, err := b.connect(t.Context())
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("connect err = %v, want ErrDisabled", err)
	}
	if sess != nil {
		t.Error("connect handed a session to its caller for a disabled backend")
	}
	b.mu.Lock()
	installed := b.session
	b.mu.Unlock()
	if installed != nil {
		t.Error("a session was installed on a disabled backend, so the next dispatch would write to it")
	}
	if got := b.Health().State; got != StateDisabled {
		t.Errorf("state = %q, want %q: the handshake overwrote the disable", got, StateDisabled)
	}
}

func TestAReadSupersededByADisableCommitsNoHealth(t *testing.T) {
	// A disable that lands between ensureSession and the generation sample is
	// invisible to the generation check, because the sample is already the
	// post-disable value. Committing then would post "disabled, 2 tools".
	fake := testfake.New("alpha", tool("kubectl_logs"), tool("kubectl_get"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("baseline list: %v", err)
	}
	before := b.Health()
	if err := r.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	err := b.commitList(b.Generation(), []*mcp.Tool{tool("kubectl_logs"), tool("kubectl_get")})
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("commitList err = %v, want ErrDisabled", err)
	}
	if got := b.Health(); got.ToolCount != 0 || got.State != StateDisabled {
		t.Errorf("health = %+v, want a disabled backend serving no tools", got)
	}
	if got := b.Health(); got.LastRefresh != before.LastRefresh {
		t.Error("committed a refresh timestamp for a read the disable superseded")
	}
}

func TestReconnectRefusesADisabledBackend(t *testing.T) {
	// Reconnecting a disabled backend would resurrect the child the kill switch
	// killed, so the refusal is asserted on the dial count rather than on the error.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	if err := r.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	dialsBefore := fake.Dials.Load()

	if err := r.Reconnect("alpha"); !errors.Is(err, ErrDisabled) {
		t.Errorf("reconnect err = %v, want ErrDisabled", err)
	}
	if n := fake.Dials.Load(); n != dialsBefore {
		t.Errorf("dials = %d, want %d: reconnect spawned a child behind a disabled backend", n, dialsBefore)
	}
	if got := b.Health().State; got != StateDisabled {
		t.Errorf("state = %q, want %q: reconnect cleared the user's disable", got, StateDisabled)
	}
}

func TestReconnectClearsTheBackoffWindow(t *testing.T) {
	// A backend inside its retry window ignores every dispatch until the window
	// expires. Reconnect that did not clear it would be a button that does nothing.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	b.mu.Lock()
	b.failures, b.retryAt = 4, time.Now().Add(time.Hour)
	b.health.State, b.health.LastErr = StateDown, "spawn refused"
	b.mu.Unlock()

	if err := r.Reconnect("alpha"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := b.Call(t.Context(), "kubectl_logs", nil); err != nil {
		t.Fatalf("call after a reconnect: %v", err)
	}
	if n := fake.SideEffects.Load(); n != 1 {
		t.Errorf("side effects = %d, want 1: the dispatch was still inside the backoff window", n)
	}
	if got := b.Health().State; got != StateUp {
		t.Errorf("state = %q, want %q", got, StateUp)
	}
}

// parkWrites parks the daemon's side of the connection just before a request
// reaches the transport, so a test can hold a dispatch that has passed its
// enabled check but written nothing.
type parkWrites struct {
	inner mcp.Transport
	park  func(method string)
}

func (t *parkWrites) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &parkWritesConn{Connection: conn, park: t.park}, nil
}

type parkWritesConn struct {
	mcp.Connection
	park func(method string)
}

func (c *parkWritesConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok {
		c.park(req.Method)
	}
	return c.Connection.Write(ctx, msg)
}

func countCalls(received []string) int {
	n := 0
	for _, r := range received {
		if r == "tools/call:open_pull_request" {
			n++
		}
	}
	return n
}

func overridesAt(t *testing.T, path string) *Overrides {
	t.Helper()
	ov, err := LoadOverrides(path, testfake.PermissiveDeclarations{})
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	return ov
}

// Scenario (backend-management spec, "An added backend serves tools without a restart"):
// Add publishes a backend that routes and lists immediately.
func TestAnAddedBackendServesWithoutARestart(t *testing.T) {
	existing := testfake.New("alpha", tool("kubectl_logs"))
	added := testfake.New("beta", tool("open_pull_request"))
	r := wire(t, Hooks{}, existing)

	if _, ok := r.Get("beta"); ok {
		t.Fatal("beta was already registered")
	}
	r.Add("beta", config.Backend{Name: "beta", Command: "unused"}, true)
	b, ok := r.Get("beta")
	if !ok {
		t.Fatal("beta not registered after Add")
	}
	b.dial = added.Transport
	t.Cleanup(func() { b.closeSession(); added.Close() })

	tools, err := b.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools on an added backend: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "open_pull_request" {
		t.Errorf("tools = %v, want the added backend's own tool", tools)
	}
	if !slices.Contains(r.Names(), "beta") {
		t.Errorf("Names() = %v, want beta included", r.Names())
	}
	if got := r.Health()["alpha"].State; got == StateDisabled {
		t.Error("adding a backend disturbed an existing one")
	}
}

// Scenario: "A removed backend stops serving". The teardown is terminal, so nothing can
// respawn a child for a name that is no longer declared.
func TestARemovedBackendStopsServing(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	r.Remove("alpha")

	if _, ok := r.Get("alpha"); ok {
		t.Error("a removed backend is still routable")
	}
	if slices.Contains(r.Names(), "alpha") {
		t.Errorf("Names() = %v, want alpha gone", r.Names())
	}
	if _, err := b.ListTools(context.Background()); !errors.Is(err, ErrShutdown) {
		t.Errorf("ListTools after Remove = %v, want ErrShutdown: the teardown must be terminal", err)
	}
}

// Scenario: "A backend added over a stale disabled flag starts enabled". Add takes the
// state from its caller and never reads the override store, which is what stops a stale
// entry from disabling a freshly declared backend.
func TestAddTakesItsEnabledStateFromItsCaller(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "overrides.json")
	ov := overridesAt(t, statePath)
	if err := ov.set("beta", true, config.Identity{Transport: "stdio"}); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	r := NewRegistry(stdioConfig("alpha"), ov, Hooks{})

	r.Add("beta", config.Backend{Name: "beta", Command: "unused"}, true)
	if got := r.Health()["beta"].State; got == StateDisabled {
		t.Errorf("beta state = %q, want enabled: Add must not consult the override store", got)
	}

	r.Add("gamma", config.Backend{Name: "gamma", Command: "unused"}, false)
	if got := r.Health()["gamma"].State; got != StateDisabled {
		t.Errorf("gamma state = %q, want %q: a replacement's captured state was dropped", got, StateDisabled)
	}
}

// Scenario: "An add during shutdown leaves no surviving child". Shutdown latches the
// registry and snapshots under the same lock, so a backend published afterwards cannot
// slip past the walk. The per-backend latch cannot cover this: the dangerous backend is
// one that was never in the map when shutdown read it.
func TestShutdownLatchesTheRegistryAgainstALaterAdd(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)

	r.Shutdown()

	if !r.ShuttingDown() {
		t.Fatal("ShuttingDown() is false after Shutdown()")
	}
	// A caller that ignores the latch must still not end up with a live child: the
	// backend it publishes is born latched and refuses to dial.
	r.Add("beta", config.Backend{Name: "beta", Command: "unused"}, true)
	b, ok := r.Get("beta")
	if !ok {
		t.Fatal("beta not registered")
	}
	if _, err := b.ListTools(context.Background()); !errors.Is(err, ErrShutdown) {
		t.Errorf("dial of a backend added after shutdown = %v, want ErrShutdown", err)
	}
}

// Shutdown and a concurrent add must not race the backend map. Run under -race.
func TestShutdownDoesNotRaceAnAdd(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	r := wire(t, Hooks{}, fake)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.Shutdown() }()
	go func() {
		defer wg.Done()
		for i := range 20 {
			name := "b" + string(rune('a'+i))
			if r.ShuttingDown() {
				return
			}
			r.Add(name, config.Backend{Name: name, Command: "unused"}, true)
		}
	}()
	wg.Wait()

	for name, h := range r.Health() {
		if h.State == StateUp {
			t.Errorf("backend %q is up after shutdown", name)
		}
	}
}
