package backend

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
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

func TestBackendUsesResolvedChildEnvironment(t *testing.T) {
	spec := config.Backend{Name: "alpha", Command: "unused", Env: map[string]string{"API_TOKEN": "prefix-${TOKEN}"}}
	b := newBackend("alpha", spec, Hooks{ResolveSecrets: func(_ context.Context, consumer config.SecretConsumer) (map[string]string, error) {
		if consumer.Name != "alpha" || len(consumer.References) != 1 || consumer.References[0] != "TOKEN" {
			t.Fatalf("consumer = %#v", consumer)
		}
		return map[string]string{"TOKEN": "resolved"}, nil
	}})
	transport, err := b.stdioTransport(t.Context())
	if err != nil {
		t.Fatalf("stdioTransport: %v", err)
	}
	command := transport.(*mcp.CommandTransport).Command
	if !slices.Contains(command.Env, "API_TOKEN=prefix-resolved") {
		t.Fatalf("child environment = %#v", command.Env)
	}
}

func TestHTTPBackendUsesResolvedHeaders(t *testing.T) {
	spec := config.Backend{Name: "alpha", HTTPURL: "https://example.test", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}}
	b := newBackend("alpha", spec, Hooks{ResolveSecrets: func(context.Context, config.SecretConsumer) (map[string]string, error) {
		return map[string]string{"TOKEN": "resolved"}, nil
	}})
	transport, err := b.httpTransport(t.Context())
	if err != nil {
		t.Fatalf("httpTransport: %v", err)
	}
	streamable := transport.(*mcp.StreamableClientTransport)
	headers := streamable.HTTPClient.Transport.(headerTransport).headers
	if headers["Authorization"] != "Bearer resolved" {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestProviderFailureIsolatesDependents(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{
		"dependent": {Name: "dependent", Command: "unused", Env: map[string]string{"TOKEN": "${TOKEN}"}},
		"unrelated": {Name: "unrelated", Command: "unused"},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{
		ResolveSecrets: func(context.Context, config.SecretConsumer) (map[string]string, error) {
			return nil, errors.New("provider unavailable")
		},
	})
	r.MarkSecretPending(config.SecretConsumer{Kind: config.ConsumerBackend, Name: "dependent"}, "unavailable")
	health := r.Health()
	if health["dependent"].State != StatePending || health["dependent"].LastErr != "pending secret resolution: unavailable" {
		t.Fatalf("dependent health = %#v", health["dependent"])
	}
	if health["unrelated"].State != StateDown || health["unrelated"].LastErr != "" {
		t.Fatalf("unrelated health = %#v", health["unrelated"])
	}
}

func TestAddedBackendCanResolveSecretsOnFirstDial(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{}}
	var resolutions atomic.Int32
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{
		ResolveSecrets: func(_ context.Context, consumer config.SecretConsumer) (map[string]string, error) {
			resolutions.Add(1)
			return map[string]string{consumer.References[0]: "resolved"}, nil
		},
	})
	r.Add("added", config.Backend{Command: "true", Env: map[string]string{"TOKEN": "${TOKEN}"}}, true)
	b, _ := r.Get("added")
	_, _ = b.ListTools(t.Context())
	if got := resolutions.Load(); got != 1 {
		t.Fatalf("secret resolutions = %d, want 1", got)
	}
}

func TestPreparedSecretsOnlyReleasePendingHealth(t *testing.T) {
	b := newBackend("alpha", config.Backend{Command: "unused", Env: map[string]string{"TOKEN": "${TOKEN}"}}, Hooks{
		ResolveSecrets: func(context.Context, config.SecretConsumer) (map[string]string, error) {
			return nil, errors.New("unexpected resolver call")
		},
	})
	for _, state := range []State{StateUp, StateNeedsAuth} {
		b.health.State = state
		if !b.prepareSecrets(map[string]string{"TOKEN": "resolved"}) {
			t.Fatalf("prepareSecrets refused state %q", state)
		}
		if got := b.Health().State; got != state {
			t.Fatalf("state after preparing secrets = %q, want %q", got, state)
		}
	}
	b.health.State = StatePending
	b.health.LastErr = "pending secret resolution: unavailable"
	if !b.prepareSecrets(map[string]string{"TOKEN": "resolved"}) {
		t.Fatal("prepareSecrets refused pending state")
	}
	if health := b.Health(); health.State != StateDown || health.LastErr != "" {
		t.Fatalf("health after pending resolution = %#v", health)
	}
	b.markSecretPending("stale startup snapshot")
	if health := b.Health(); health.State != StateDown || health.LastErr != "" {
		t.Fatalf("stale pending snapshot changed health = %#v", health)
	}
	values, err := b.secretValues(t.Context())
	if err != nil {
		t.Fatalf("secretValues: %v", err)
	}
	if values["TOKEN"] != "resolved" {
		t.Fatalf("prepared values = %#v", values)
	}
	clear(values)
}

func TestReconnectRetriesPendingSecretResolutionBeforeRefresh(t *testing.T) {
	var events []string
	cfg := &config.Config{Backends: map[string]config.Backend{
		"alpha": {Command: "unused", Env: map[string]string{"TOKEN": "${TOKEN}"}},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{
		RetrySecrets: func() { events = append(events, "retry") },
		Refresh:      func(string) { events = append(events, "refresh") },
	})
	r.MarkSecretPending(config.SecretConsumer{Kind: config.ConsumerBackend, Name: "alpha"}, "interaction_required")
	if err := r.Reconnect("alpha"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	if got := strings.Join(events, ","); got != "retry,refresh" {
		t.Fatalf("events = %q, want retry,refresh", got)
	}
}

func TestInteractiveBudgetSurvivesConfiguredTimeout(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{
		"alpha": {Name: "alpha", Command: "true", TimeoutSec: 1},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")
	budget := make(chan time.Duration, 1)
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("handshake context has no deadline")
			d = time.Now()
		}
		budget <- time.Until(d)
		return nil, errors.New("refused")
	}

	b.ExpectAuthorization()
	_, _ = b.ListTools(t.Context())
	if got := <-budget; got < 4*time.Minute {
		t.Errorf("handshake budget = %v; a configured timeout must not cap the interactive budget", got)
	}
}

func TestAbortedHandshakeLeavesAPendingInteractiveFlag(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{
		"alpha": {Name: "alpha", Command: "true"},
	}}
	r := NewRegistry(cfg, overridesAt(t, filepath.Join(t.TempDir(), "overrides.json")), Hooks{})
	b, _ := r.Get("alpha")

	entered := make(chan struct{})
	release := make(chan struct{})
	budgets := make(chan time.Duration, 2)
	var calls atomic.Int32
	b.dial = func(ctx context.Context) (mcp.Transport, error) {
		d, _ := ctx.Deadline()
		budgets <- time.Until(d)
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil, errors.New("refused")
	}

	// A background refresh is mid-handshake when the user asks to authorize.
	done := make(chan struct{})
	go func() { defer close(done); _, _ = b.ListTools(t.Context()) }()
	<-entered
	b.ExpectAuthorization()
	close(release)
	<-done
	<-budgets // the background attempt's budget is not under test

	// The authorize path's reconnect clears backoff before the next handshake;
	// done directly here to keep the test on the flag, not the backoff.
	b.mu.Lock()
	b.failures, b.retryAt = 0, time.Time{}
	b.mu.Unlock()

	_, _ = b.ListTools(t.Context())
	if got := <-budgets; got < 4*time.Minute {
		t.Errorf("the user's handshake got %v, want the interactive budget: the aborted attempt must not clear the flag", got)
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
		// Any server-initiated request exercises the same invariant; elicitation is one
		// the daemon's client never advertises, so the shared session must refuse it.
		_, err := req.Session.Elicit(ctx, nil)
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

// A streamable backend must reach its upstream over HTTP/1.1. One real upstream
// withholds the standalone SSE stream's response headers for a full minute over
// HTTP/2 and then resets the stream, which fails the handshake and takes the whole
// backend down, while answering that same GET immediately over HTTP/1.1.
func TestAStreamableBackendDoesNotNegotiateHTTP2(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.Proto:
		default:
		}
	}))
	// Offered by the server, so HTTP/1.1 is the client's choice and not the only option.
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := streamableBase.Clone()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool

	res, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	res.Body.Close()

	if got := <-seen; got != "HTTP/1.1" {
		t.Errorf("upstream saw %s, want HTTP/1.1", got)
	}
}

// A real upstream answers its 401 with `Bearer realm="mcp" resource_metadata="..."`, with
// no comma, which RFC 9110 does not permit and the SDK's parser refuses. Refusing it here
// too would mean that upstream can never be authorized at all, so the transport repairs it
// on the way in. Asserted through the SDK's own parser rather than against an expected
// string, because interoperating with that parser is the entire point, and the first
// assertion retires this repair on its own once the parser grows to accept the malformed
// form itself.
func TestAChallengeMissingItsCommaIsRepairedForTheSDKParser(t *testing.T) {
	const challenge = `Bearer realm="mcp" resource_metadata="https://example.test/.well-known/oauth-protected-resource/api/mcp"`
	if _, err := oauthex.ParseWWWAuthenticate([]string{challenge}); err == nil {
		t.Skip("the SDK parser now accepts a challenge with no comma between its parameters; this repair is obsolete")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// Through the transport a backend actually dials with, so the repair is proven to be
	// installed on the response path and not merely to exist.
	res, err := (&http.Client{Transport: headerTransport{base: streamableBase}}).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	res.Body.Close()

	got, err := oauthex.ParseWWWAuthenticate(res.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	if err != nil {
		t.Fatalf("the repaired challenge is still unparseable: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parsed %d challenges, want 1: %+v", len(got), got)
	}
	// The pointer to the protected-resource metadata is the parameter the whole flow
	// hangs on: without it there is no discovery and no authorization.
	const want = "https://example.test/.well-known/oauth-protected-resource/api/mcp"
	if got[0].Params["resource_metadata"] != want {
		t.Errorf("resource_metadata = %q, want %q", got[0].Params["resource_metadata"], want)
	}
	if got[0].Params["realm"] != "mcp" {
		t.Errorf("realm = %q, want mcp: the repair dropped the parameter it split from", got[0].Params["realm"])
	}
}

// A well-formed challenge is left exactly as it arrived, so the repair above cannot
// corrupt the upstreams that were already correct.
func TestAWellFormedChallengeIsUntouched(t *testing.T) {
	const good = `Bearer realm="mcp", resource_metadata="https://example.test/.well-known/x", error="invalid_token"`
	h := http.Header{"Www-Authenticate": []string{good}}
	repairChallengeHeader(h)
	if got := h.Get("Www-Authenticate"); got != good {
		t.Errorf("challenge was rewritten:\n got %s\nwant %s", got, good)
	}
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
