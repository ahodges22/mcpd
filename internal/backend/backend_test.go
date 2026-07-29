package backend

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/testfake"
)

func TestCallReachesTheOwningBackendOverOneNewSession(t *testing.T) {
	alpha := testfake.New("alpha", tool("kubectl_logs"))
	beta := testfake.New("beta", tool("kubectl_logs"))
	r := wire(t, Hooks{}, alpha, beta)

	b, ok := r.Get("alpha")
	if !ok {
		t.Fatal("alpha not registered")
	}
	res, err := b.Call(t.Context(), "kubectl_logs", map[string]any{"pod": "p"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := textOf(t, res); got != "ok:alpha" {
		t.Errorf("answered by %q, want ok:alpha", got)
	}
	if n := beta.SideEffects.Load(); n != 0 {
		t.Errorf("beta side effects = %d, want 0", n)
	}
	if n := alpha.SideEffects.Load(); n != 1 {
		t.Errorf("alpha side effects = %d, want 1", n)
	}
	if n := alpha.Dials.Load(); n != 1 {
		t.Errorf("alpha dials = %d, want 1: a call with no session reconnects and sends once", n)
	}
}

func TestConnectFailureIsReportedRetryable(t *testing.T) {
	r := NewRegistry(stdioConfig("alpha"), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	b.dial = func(context.Context) (mcp.Transport, error) { return nil, errors.New("spawn refused") }

	_, err := b.Call(t.Context(), "kubectl_logs", nil)
	if err == nil {
		t.Fatal("expected an error when establishment fails")
	}
	if !errors.Is(err, ErrNotAttempted) {
		t.Errorf("err = %v, want ErrNotAttempted: with no connection nothing can have reached the upstream", err)
	}
}

func TestConcurrentCallsShareOneSession(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	release := make(chan struct{})
	fake.OnCall = func(context.Context, *mcp.CallToolRequest) { <-release }
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")

	const clients = 8
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Call(t.Context(), "kubectl_logs", nil); err != nil {
				errs <- err
			}
		}()
	}
	waitFor(t, func() bool { return fake.SideEffects.Load() == clients })
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("call: %v", err)
	}

	if n := fake.Dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1: %d clients must multiplex onto one session", n, clients)
	}
}

func TestLostResponseAfterSideEffectIsNotReplayed(t *testing.T) {
	fake := testfake.New("alpha", tool("open_pull_request"))
	acted, done := holdAfterSideEffect(fake)
	defer done()

	// The upstream commits the side effect, then the connection dies before the
	// response comes back.
	r := wireDelivering(t, fake, func(conn mcp.Connection) error {
		acted()
		return conn.Close()
	})
	b, _ := r.Get("alpha")

	_, err := b.Call(t.Context(), "open_pull_request", nil)
	assertOutcomeUnknown(t, err, "open_pull_request")
	if n := fake.SideEffects.Load(); n != 1 {
		t.Errorf("side effects = %d, want exactly 1", n)
	}
}

func TestWriteErrorAfterDeliveryIsNotReplayed(t *testing.T) {
	fake := testfake.New("alpha", tool("open_pull_request"))
	acted, done := holdAfterSideEffect(fake)
	defer done()

	// The bytes reach the upstream and it acts on them; only then does the
	// daemon's write report an error. Drawing the boundary at the write's return
	// value would replay this call and open the pull request twice.
	r := wireDelivering(t, fake, func(mcp.Connection) error {
		acted()
		return errors.New("write failed after the request was delivered")
	})
	b, _ := r.Get("alpha")

	_, err := b.Call(t.Context(), "open_pull_request", nil)
	assertOutcomeUnknown(t, err, "open_pull_request")
	if n := fake.SideEffects.Load(); n != 1 {
		t.Errorf("side effects = %d, want exactly 1 (the call must not be replayed)", n)
	}
}

// holdAfterSideEffect makes the fake commit its side effect and then park,
// holding its response back. acted blocks until that has happened; done lets the
// upstream finish. Without the hold, jsonrpc2 retires the call with the response
// that already arrived, the write's error is never observed, and the test would
// pass against an implementation that replays.
func holdAfterSideEffect(fake *testfake.Fake) (acted, done func()) {
	committed := make(chan struct{}, 4)
	release := make(chan struct{})
	fake.OnCall = func(context.Context, *mcp.CallToolRequest) {
		committed <- struct{}{}
		<-release
	}
	return func() {
		select {
		case <-committed:
		case <-time.After(5 * time.Second):
			panic("the request never reached the upstream, so this test proves nothing")
		}
	}, func() { close(release) }
}

func TestStdioChildEnvIsExplicitlyConstructed(t *testing.T) {
	restore := environ
	t.Cleanup(func() { environ = restore })
	environ = func() []string {
		return []string{"PATH=/usr/bin", "HOME=/home/u", "AWS_PROFILE=dev", "GH_PAT=secret"}
	}

	cfg := &config.Config{Backends: map[string]config.Backend{
		"alpha": {Name: "alpha", Command: "true", EnvPassthrough: []string{"AWS_*"}},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")

	tr, err := b.dial(t.Context())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cmd, ok := tr.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("transport is %T, want *mcp.CommandTransport", tr)
	}
	if cmd.Command.Env == nil {
		t.Fatal("cmd.Env is nil, so the child would inherit every credential mcpd holds")
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u", "AWS_PROFILE=dev"} {
		if !slices.Contains(cmd.Command.Env, want) {
			t.Errorf("cmd.Env is missing %q: %v", want, cmd.Command.Env)
		}
	}
	if slices.Contains(cmd.Command.Env, "GH_PAT=secret") {
		t.Errorf("cmd.Env leaked an undeclared credential: %v", cmd.Command.Env)
	}
}

func TestUnreachableBackendRetainsItsError(t *testing.T) {
	endpoint := deadEndpoint(t)
	cfg := &config.Config{Backends: map[string]config.Backend{
		"alpha": {Name: "alpha", HTTPURL: endpoint},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")

	if _, err := b.ListTools(t.Context()); err == nil {
		t.Fatal("expected an error from an unreachable endpoint")
	}
	h := b.Health()
	if h.State != StateDown {
		t.Errorf("state = %q, want %q", h.State, StateDown)
	}
	if !strings.Contains(h.LastErr, endpoint) {
		t.Errorf("health error %q does not name the unreachable endpoint %s", h.LastErr, endpoint)
	}
}

func TestRefreshInvalidatedMidFlightDoesNotCommit(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"), tool("kubectl_get"))
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")

	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("baseline list: %v", err)
	}
	before := b.Health()
	if before.ToolCount != 2 {
		t.Fatalf("baseline tool count = %d, want 2", before.ToolCount)
	}

	// A lifecycle transition drops the shared session while the next list is in
	// flight. The list itself still succeeds, on a session that is no longer the
	// backend's, so only the generation reveals that its result is superseded.
	fake.BeforeList = func() {
		fake.SetTools(tool("kubectl_logs"))
		b.mu.Lock()
		sess := b.session
		b.mu.Unlock()
		b.dropSession(sess, nil)
	}

	_, err := b.ListTools(t.Context())
	if !errors.Is(err, ErrStaleGeneration) {
		t.Errorf("err = %v, want ErrStaleGeneration", err)
	}
	after := b.Health()
	if after.ToolCount != before.ToolCount {
		t.Errorf("committed tool count %d over %d: a result from a superseded generation must be discarded", after.ToolCount, before.ToolCount)
	}
	if !after.LastRefresh.Equal(before.LastRefresh) {
		t.Error("committed a refresh timestamp from a superseded generation")
	}
}

func TestCancelConnectAbortsAHungHandshake(t *testing.T) {
	r := NewRegistry(stdioConfig("alpha"), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	dialing := make(chan struct{})
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := b.Call(t.Context(), "kubectl_logs", nil)
		done <- err
	}()
	<-dialing
	b.cancelConnect()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotAttempted) {
			t.Errorf("err = %v, want ErrNotAttempted: an aborted handshake sent nothing", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelConnect did not abort the handshake")
	}
	if !b.life.TryLock() {
		t.Error("the lifecycle mutex is still held, so a disable would block behind the abandoned handshake")
	}
}

func TestToolListChangedReachesTheRegistryCallback(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	changed := make(chan string, 4)
	r := wire(t, Hooks{ToolListChanged: func(name string) { changed <- name }}, fake)
	b, _ := r.Get("alpha")

	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("list: %v", err)
	}
	fake.SetTools(tool("kubectl_logs"), tool("kubectl_get"))

	select {
	case name := <-changed:
		if name != "alpha" {
			t.Errorf("callback named %q, want alpha", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tool-list-changed callback; the catalog would serve a stale list")
	}
}

func TestReconnectFiresOnlyAfterASessionIsLost(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	reconnected := make(chan string, 4)
	r := wire(t, Hooks{Reconnected: func(name string) { reconnected <- name }}, fake)
	b, _ := r.Get("alpha")

	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("first list: %v", err)
	}
	select {
	case name := <-reconnected:
		t.Fatalf("reconnect hook fired for %q on the first connect, which would double every cold refresh", name)
	default:
	}

	b.closeSession()
	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("list after reconnect: %v", err)
	}

	select {
	case name := <-reconnected:
		if name != "alpha" {
			t.Errorf("hook named %q, want alpha", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect trigger; a tool list that changed while the session was down would never be re-read")
	}
}

func TestServerInitiatedRequestsAreNotForwarded(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	roots := make(chan error, 1)
	fake.OnCall = func(ctx context.Context, req *mcp.CallToolRequest) {
		_, err := req.Session.ListRoots(ctx, nil)
		roots <- err
	}
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get("alpha")

	if _, err := b.Call(t.Context(), "kubectl_logs", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := <-roots; err == nil {
		t.Error("the daemon served a server-initiated roots request; one shared session cannot attribute it to a client")
	}
}

// deliverThen wraps a transport so that a tools/call is genuinely delivered to
// the upstream before the daemon's side of the connection misbehaves. Without
// it, a test cannot reach the case that distinguishes "a send was attempted"
// from "the write returned nil".
type deliverThen struct {
	inner mcp.Transport
	after func(mcp.Connection) error
}

func (t *deliverThen) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &deliverThenConn{Connection: conn, after: t.after}, nil
}

type deliverThenConn struct {
	mcp.Connection
	after func(mcp.Connection) error
	once  sync.Once
}

func (c *deliverThenConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := c.Connection.Write(ctx, msg); err != nil {
		return err
	}
	var err error
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "tools/call" {
		c.once.Do(func() { err = c.after(c.Connection) })
	}
	return err
}

// wire builds a registry over fakes and points each backend at its fake. dial
// is a package-internal seam: a production backend dials a child process or an
// HTTP endpoint.
func wire(t *testing.T, hooks Hooks, fakes ...*testfake.Fake) *Registry {
	t.Helper()
	names := make([]string, 0, len(fakes))
	for _, f := range fakes {
		names = append(names, f.Name)
	}
	r := NewRegistry(stdioConfig(names...), overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), hooks)
	for _, f := range fakes {
		b, ok := r.Get(f.Name)
		if !ok {
			t.Fatalf("%s not registered", f.Name)
		}
		b.dial = f.Transport
		t.Cleanup(func() {
			// The client session must go first: closing the fake while mcpd still
			// holds a long-lived subscription blocks in ServerSession.Close.
			b.closeSession()
			f.Close()
		})
	}
	return r
}

// wireDelivering wires one fake behind a deliverThen wrapper.
func wireDelivering(t *testing.T, fake *testfake.Fake, after func(mcp.Connection) error) *Registry {
	t.Helper()
	r := wire(t, Hooks{}, fake)
	b, _ := r.Get(fake.Name)
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		inner, err := fake.Transport(ctx)
		if err != nil {
			return nil, err
		}
		return &deliverThen{inner: inner, after: after}, nil
	}
	return r
}

func stdioConfig(names ...string) *config.Config {
	cfg := &config.Config{Backends: make(map[string]config.Backend, len(names))}
	for _, n := range names {
		cfg.Backends[n] = config.Backend{Name: n, Command: "unused"}
	}
	return cfg
}

func assertOutcomeUnknown(t *testing.T, err error, toolName string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error when the response is lost")
	}
	if errors.Is(err, ErrNotAttempted) {
		t.Errorf("err = %v, reported as not attempted; a caller would retry a request the upstream already acted on", err)
	}
	if !strings.Contains(err.Error(), toolName) {
		t.Errorf("err = %v, does not name the tool, so the caller cannot tell what to go and check", err)
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("err = %v, does not say the outcome is unknown", err)
	}
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: name + " description"}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

// deadEndpoint returns a URL on a port that has just been released, so a dial
// fails immediately rather than depending on name resolution.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr + "/mcp"
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
