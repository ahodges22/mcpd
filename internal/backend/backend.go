// Package backend owns mcpd's upstream MCP sessions: one shared session per
// backend, with at-most-once tool dispatch.
package backend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
)

// ErrNotAttempted reports that no send was attempted, so the caller may retry.
// Every other error leaves the outcome unknown and must not be replayed: a write
// can fail after its bytes were delivered and acted on.
var ErrNotAttempted = errors.New("no send attempted")

// ErrStaleGeneration reports that a lifecycle transition superseded an operation
// while it was in flight, so its result was discarded rather than committed.
var ErrStaleGeneration = errors.New("superseded by a lifecycle transition")

// ErrDisabled reports that the backend is disabled by an override. It is not a
// backend failure: nothing was attempted and nothing is wrong upstream.
var ErrDisabled = errors.New("backend disabled")

// ErrShutdown reports that the daemon has shut this backend down. Like ErrDisabled
// it is not a backend failure, and unlike it there is no way back: the state is
// terminal for the life of the process.
var ErrShutdown = errors.New("backend shut down")

type State string

const (
	StateUp        State = "up"
	StateDown      State = "down"
	StateNeedsAuth State = "needs-auth"
	StateDisabled  State = "disabled"
)

type Health struct {
	State       State     `json:"state"`
	Transport   string    `json:"transport"`
	ToolCount   int       `json:"tool_count"`
	LastRefresh time.Time `json:"last_refresh"`
	LastErr     string    `json:"last_error,omitempty"`
	AuthNote    string    `json:"auth_note,omitempty"`
}

// ConnectAttempt is how the latest handshake ended, tagged with the generation it
// began in. The tag is what lets a waiter tell its own attempt's outcome from an
// earlier attempt's, including one its own teardown cancelled.
type ConnectAttempt struct {
	Generation uint64
	Failed     bool
}

// environ is a package var so tests can inject a parent environment.
var environ = os.Environ

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 30 * time.Second
	// defaultConnectTimeout bounds a handshake when the backend declares no
	// timeout. Unbounded, a hung spawn would hold the lifecycle mutex forever and
	// a disable could never take it.
	defaultConnectTimeout = 60 * time.Second
	// interactiveConnectTimeout is the budget for a handshake the user explicitly asked for,
	// and it is sized for a person rather than for a machine. A first authorization sends the
	// user to a provider that may make them log in, and sometimes through single sign-on,
	// before it will show a consent screen; the handshake cannot complete until they finish.
	// Bounding that by the ordinary dial budget abandons the authorization while the consent
	// screen is still open, and the code the browser eventually brings back then matches no
	// outstanding request. It matches oauthstore's own pendingWindow, which is the backstop
	// for the same wait.
	interactiveConnectTimeout = 5 * time.Minute
)

// Backend is one upstream MCP server, shared by every connected client.
//
// Lock order is transition, then gate, then life, then mu. A disable takes gate
// exclusively before life, so dispatch must never acquire them the other way
// round.
type Backend struct {
	name        string
	spec        config.Backend
	client      *mcp.Client
	dial        func(context.Context) (mcp.Transport, error)
	onReconnect func(server string)
	stopRefresh func(server string)
	dropTools   func(server string)
	refresh     func(server string)
	authHandler func(server string) (auth.OAuthHandler, error)
	// authNote is what the auth column says when nothing is pending, so a note left
	// by an authorization does not outlive it.
	authNote string

	// transition serializes a whole user-initiated enable or disable, including
	// the override write, which happens before the gate closes.
	transition sync.Mutex
	gate       sync.RWMutex // RLock is a dispatch lease; Lock closes and drains it
	life       sync.Mutex   // serializes lifecycle transitions
	// gen advances only while mu is held, which is what lets a read check it and
	// write the health record in one critical section rather than straddling a
	// transition and posting a tool count over a backend that stopped serving them.
	gen atomic.Uint64

	mu             sync.Mutex
	session        *mcp.ClientSession
	health         Health
	failures       int
	retryAt        time.Time
	connectCancel  context.CancelFunc
	connectAttempt ConnectAttempt
	connected      bool // a session has existed, so the next connect is a reconnect
	// interactive latches that the next handshake is one a user asked for and is waiting on
	// in a browser, so it gets the budget above. Atomic rather than guarded by mu, because
	// connectTimeout is read with mu already held.
	interactive atomic.Bool

	// stopping and connectCancel share one critical section so teardown cannot miss a new handshake.
	stopping bool
	// shutdown latches for the life of the process: the daemon is exiting, so nothing
	// may dial again however it asks. stopping cannot serve, being cleared by the very
	// transitions this has to survive.
	shutdown bool
}

// Generation changes on every lifecycle transition, so an operation that
// started before one can discard its result rather than commit it.
func (b *Backend) Generation() uint64 { return b.gen.Load() }

func (b *Backend) Health() Health {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.health
}

// ConnectAttempt reports the latest handshake's outcome and the current generation from
// one critical section, so a caller cannot pair an attempt with a generation it never
// ran in. gen advances only under mu, which is what makes that possible.
func (b *Backend) ConnectAttempt() (ConnectAttempt, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connectAttempt, b.gen.Load()
}

// UsesOAuth reports whether this backend authorizes with OAuth, which is what
// makes the authenticate action meaningful for it.
func (b *Backend) UsesOAuth() bool { return b.spec.Auth == "oauth" }

func (b *Backend) shuttingDown() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shutdown
}

// NoteNeedsAuth records that this backend cannot serve until the user authorizes
// it, with note rendered verbatim as its auth note. A disabled backend is left
// alone: the kill switch outranks every other state.
func (b *Backend) NoteNeedsAuth(note string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.health.State == StateDisabled {
		return
	}
	b.health.State = StateNeedsAuth
	b.health.AuthNote = note
}

// NoteAuthorized clears a needs-auth marking once an authorization has succeeded.
// An authorization can complete on a session that never went down, and then no
// handshake runs to record that the backend is serving again.
func (b *Backend) NoteAuthorized() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.health.State != StateNeedsAuth {
		return
	}
	b.health.AuthNote = b.authNote
	if b.session != nil {
		b.health.State = StateUp
		return
	}
	// No session yet: the handshake this authorization unblocked is the thing that
	// will say up, and down is what the backend is until it does.
	b.health.State = StateDown
}

// Call dispatches a tool call at most once. It returns an error wrapping
// ErrNotAttempted only when no send began; any other error means the upstream
// may have received and acted on the request.
func (b *Backend) Call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	b.gate.RLock()
	defer b.gate.RUnlock()

	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	sess, err := b.ensureSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: tool %s: %w", ErrNotAttempted, tool, err)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("tool %s: outcome unknown, the backend may have executed it: %w", tool, err)
	}
	return res, nil
}

// ListTools takes no dispatch lease: a slow refresh must not block a lifecycle
// transition, and a stale result is caught by the generation counter instead.
func (b *Backend) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	sess, err := b.ensureSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %w", b.name, err)
	}
	gen := b.gen.Load()
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %s: list tools: %w", b.name, b.noteListFailure(err))
	}
	if err := b.commitList(gen, res.Tools); err != nil {
		return nil, fmt.Errorf("backend %s: list tools: %w", b.name, err)
	}
	return res.Tools, nil
}

// commitList records a completed read, or reports why it was discarded. The
// generation check and the health write are one critical section, so a read
// superseded mid-flight discards its result rather than committing it.
//
// The state is checked as well as the generation, because a disable that lands
// between ensureSession and the generation sample is invisible to the generation
// check: that read's sample is already the post-disable value.
func (b *Backend) commitList(gen uint64, tools []*mcp.Tool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.health.State == StateDisabled {
		return ErrDisabled
	}
	if b.gen.Load() != gen {
		return ErrStaleGeneration
	}
	b.health.ToolCount = len(tools)
	b.health.LastRefresh = time.Now()
	b.health.LastErr = ""
	return nil
}

func (b *Backend) ensureSession(ctx context.Context) (*mcp.ClientSession, error) {
	b.mu.Lock()
	sess, state := b.session, b.health.State
	b.mu.Unlock()

	if state == StateDisabled {
		return nil, ErrDisabled
	}
	if sess != nil {
		return sess, nil
	}
	return b.connect(ctx)
}

func (b *Backend) connect(ctx context.Context) (*mcp.ClientSession, error) {
	b.life.Lock()
	defer b.life.Unlock()

	b.mu.Lock()
	if b.health.State == StateDisabled {
		b.mu.Unlock()
		return nil, ErrDisabled
	}
	// The one place a child is ever spawned, so refusing here is what makes the
	// shutdown latch a barrier rather than a hint.
	if b.shutdown {
		b.mu.Unlock()
		return nil, ErrShutdown
	}
	if sess := b.session; sess != nil {
		b.mu.Unlock()
		return sess, nil
	}
	if b.stopping {
		b.mu.Unlock()
		return nil, fmt.Errorf("backend %s: a lifecycle transition is in progress", b.name)
	}
	if wait := time.Until(b.retryAt); wait > 0 {
		last := b.health.LastErr
		b.mu.Unlock()
		return nil, fmt.Errorf("backing off %s after: %s", wait.Round(time.Millisecond), last)
	}
	ctx, cancel := context.WithTimeout(ctx, b.connectTimeout())
	b.connectAttempt = ConnectAttempt{Generation: b.gen.Load()}
	b.connectCancel = cancel
	b.mu.Unlock()
	defer func() {
		cancel()
		b.interactive.Store(false)
		b.mu.Lock()
		b.connectCancel = nil
		b.mu.Unlock()
	}()

	transport, err := b.dial(ctx)
	if err != nil {
		return nil, b.failConnect("build transport", err)
	}
	sess, err := b.client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, b.failConnect("connect", err)
	}

	b.mu.Lock()
	if b.health.State == StateDisabled {
		// A disable landed while this handshake ran. Installing the session now
		// would leave a live child behind a backend the user turned off.
		b.mu.Unlock()
		sess.Close()
		return nil, ErrDisabled
	}
	b.session = sess
	b.failures = 0
	b.retryAt = time.Time{}
	b.health.State = StateUp
	b.health.LastErr = ""
	b.health.AuthNote = b.authNote
	reconnected := b.connected
	b.connected = true
	b.gen.Add(1)
	b.mu.Unlock()

	go b.watch(sess)
	// Only a reconnect is a trigger. Firing on the first connect would double every
	// cold refresh, because it is the refresh's own read that opens the session.
	if reconnected && b.onReconnect != nil {
		go b.onReconnect(b.name)
	}
	return sess, nil
}

func (b *Backend) watch(sess *mcp.ClientSession) {
	b.dropSession(sess, sess.Wait())
}

// cancelConnect aborts an in-flight handshake. A caller that takes life after
// calling it is guaranteed the aborted handshake has finished, because connect
// holds life for its whole duration.
func (b *Backend) cancelConnect() {
	b.mu.Lock()
	cancel := b.connectCancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ConnectTimeout reports the handshake budget this backend is configured to
// allow. A caller that imposes its own deadline on an operation which may have to
// connect must allow at least this much on top of its own budget, or it silently
// truncates a handshake the configuration permits. A cold `npx` fetch is the case
// this exists for.
func (b *Backend) ConnectTimeout() time.Duration { return b.connectTimeout() }

func (b *Backend) connectTimeout() time.Duration {
	configured := defaultConnectTimeout
	if b.spec.TimeoutSec > 0 {
		configured = time.Duration(b.spec.TimeoutSec) * time.Second
	}
	if b.interactive.Load() {
		return max(configured, interactiveConnectTimeout)
	}
	return configured
}

// ExpectAuthorization tells this backend that its next handshake is one the user asked for
// and is waiting on in a browser, so it is given a budget a person can meet. It is cleared
// when that handshake finishes, however it finishes.
func (b *Backend) ExpectAuthorization() { b.interactive.Store(true) }

// teardownMode distinguishes the kill switch from a reconnect. Only the kill
// switch latches StateDisabled and evicts the tools: a reconnect leaves the
// entries in place, because a down backend deliberately keeps them and a
// vanish-and-reappear would churn every connected pass-through client.
//
// forShutdown is a reconnect's teardown that nothing follows: the tools stay in the
// persisted catalog for the next start to serve, no override is written, any disabled
// marking is left alone, and the shutdown latch is set. The latch is what makes it
// terminal, refusing every dial, while beginTransition refuses every transition.
type teardownMode int

const (
	forDisable teardownMode = iota
	forReconnect
	forShutdown
)

// teardown follows transition, gate, life, mu, taking mu before the gate but never across
// it; stopping precedes cancellation, refresh stops before life, and tools drop last.
func (b *Backend) teardown(mode teardownMode) {
	b.mu.Lock()
	b.stopping = true
	b.mu.Unlock()
	b.cancelConnect()

	b.gate.Lock()
	defer b.gate.Unlock()

	b.mu.Lock()
	if mode == forDisable {
		b.health.State = StateDisabled
		b.health.ToolCount = 0
	} else if b.health.State != StateDisabled {
		// Only a shutdown reaches here with the kill switch latched, since a reconnect
		// refuses a disabled backend, and a shutdown must not lift it.
		b.health.State = StateDown
	}
	// Never cleared: the latch is what makes a shutdown terminal, so no later
	// transition of any mode can dial a child this process would not own.
	b.shutdown = b.shutdown || mode == forShutdown
	b.health.LastErr = ""
	b.gen.Add(1)
	b.mu.Unlock()

	b.cancelConnect()
	if b.stopRefresh != nil {
		b.stopRefresh(b.name)
	}

	b.life.Lock()
	b.closeSession()
	b.mu.Lock()
	if mode == forReconnect {
		// The cancelled handshake may have re-armed backoff after teardown cleared it.
		b.failures, b.retryAt = 0, time.Time{}
	}
	b.stopping = false
	b.mu.Unlock()
	b.life.Unlock()

	if mode == forDisable && b.dropTools != nil {
		b.dropTools(b.name)
	}
}

// restore re-enables the backend, under life so it cannot race a teardown's
// closeSession and leave the backend enabled with a child the teardown then
// kills, or a second child spawned behind the first.
func (b *Backend) restore() {
	b.life.Lock()
	b.mu.Lock()
	disabled := b.health.State == StateDisabled
	if disabled {
		b.health.State = StateDown
		b.health.LastErr = ""
		b.failures, b.retryAt = 0, time.Time{}
		b.gen.Add(1)
	}
	b.mu.Unlock()
	b.life.Unlock()
	if !disabled {
		return
	}
	if b.refresh != nil {
		b.refresh(b.name)
	}
}

// closeSession ends the shared session if there is one, which for a stdio backend
// terminates its child: the SDK's command transport closes stdin, then signals,
// then kills.
func (b *Backend) closeSession() {
	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()
	if sess == nil {
		return
	}
	sess.Close()
	b.dropSession(sess, nil)
}

// dropSession clears the shared session so the next dispatch reconnects instead
// of writing to a dead transport.
func (b *Backend) dropSession(sess *mcp.ClientSession, cause error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != sess {
		return
	}
	b.session = nil
	if b.health.State != StateDisabled {
		b.health.State = StateDown
		if cause != nil {
			b.health.LastErr = cause.Error()
		}
	}
	b.gen.Add(1)
}

// failConnect records a failed handshake and reports it. A handshake our own
// disable aborted is not a backend failure: it reports ErrDisabled and leaves the
// health record saying disabled.
func (b *Backend) failConnect(stage string, cause error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Only connect writes this record, and it holds life throughout, so this can only
	// be marking its own attempt.
	b.connectAttempt.Failed = true
	if b.health.State == StateDisabled {
		return ErrDisabled
	}
	b.failures++
	// A handshake that stopped for want of an authorization is not a backend
	// failure: needs-auth is what tells the user to act, and StateDown would bury it.
	if b.health.State != StateNeedsAuth {
		b.health.State = StateDown
	}
	b.health.LastErr = cause.Error()
	b.retryAt = time.Now().Add(min(backoffBase<<min(b.failures-1, 8), backoffMax))
	// Logged as well as recorded, because the health record holds only the latest failure.
	// A handshake that fails and is immediately retried overwrites the cause of the first
	// with the cause of the second, and the first is the one that explains the second.
	slog.Warn("handshake failed", "server", b.name, "stage", stage, "failures", b.failures, "error", cause)
	return fmt.Errorf("%s: %w", stage, cause)
}

// noteListFailure records a failed read and reports it. A read our own disable
// broke is not a backend failure: it reports ErrDisabled and leaves the health
// record saying disabled, so the status surface cannot show a backend the user
// turned off as a broken one.
func (b *Backend) noteListFailure(cause error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.health.State == StateDisabled {
		return ErrDisabled
	}
	b.health.LastErr = cause.Error()
	return cause
}

func (b *Backend) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.spec.TimeoutSec <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(b.spec.TimeoutSec)*time.Second)
}

func (b *Backend) stdioTransport(context.Context) (mcp.Transport, error) {
	cmd := exec.Command(b.spec.Command, b.spec.Args...)
	// Never nil: nil means inherit-everything, which would hand every credential
	// mcpd holds to third-party code.
	cmd.Env = b.spec.ChildEnv(environ())
	return &mcp.CommandTransport{Command: cmd}, nil
}

// streamableBase is the base RoundTripper for every streamable HTTP backend. It
// deliberately does not negotiate HTTP/2, because an upstream that mishandles the
// standalone SSE stream over HTTP/2 costs the whole backend while HTTP/2 itself buys
// nothing here: a backend holds one or two long-lived streams, so there is no
// concurrency for multiplexing to win back. One observed upstream withholds the SSE
// response headers for a full minute over HTTP/2 and then resets the stream, while
// answering immediately over HTTP/1.1.
var streamableBase = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	// Named rather than left to the field interaction above, so the ALPN offer itself
	// carries the decision and a later clone cannot quietly restore h2.
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return t
}()

func (b *Backend) httpTransport(context.Context) (mcp.Transport, error) {
	t := &mcp.StreamableClientTransport{
		Endpoint: b.spec.HTTPURL,
		// No http.Client.Timeout: it would also cap the long-lived standalone SSE
		// stream. Per-call deadlines come from withTimeout instead.
		HTTPClient: &http.Client{Transport: headerTransport{
			base:    streamableBase,
			headers: b.spec.ExpandHeaders(environ()),
		}},
	}
	if b.spec.Auth == "oauth" {
		if b.authHandler == nil {
			return nil, fmt.Errorf("backend %s declares oauth but no authorization handler is configured", b.name)
		}
		h, err := b.authHandler(b.name)
		if err != nil {
			return nil, err
		}
		t.OAuthHandler = h
	}
	return t, nil
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return res, err
	}
	repairChallengeHeader(res.Header)
	return res, nil
}

// missingComma matches a quoted auth-param value followed by another parameter with no
// comma between them, which is the one malformation seen in the wild. RE2 has no
// lookahead, so the following parameter name is captured and put back rather than merely
// required, which is why the caller has to iterate: one pass consumes the name it matched,
// and a second gap immediately after it is only reachable on the next.
var missingComma = regexp.MustCompile(`(="[^"]*")[ \t]+([A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+[ \t]*=)`)

// repairChallengeHeader inserts the commas RFC 9110 requires between the auth-params of a
// WWW-Authenticate challenge. One upstream sends `Bearer realm="mcp" resource_metadata="..."`,
// which is malformed, and the SDK's parser rightly refuses it; refusing it here too would
// mean that upstream can never be authorized at all. A well-formed header already has the
// comma, so this leaves it untouched. Nothing is loosened by this: the repaired header goes
// through exactly the same parser and the same checks.
func repairChallengeHeader(h http.Header) {
	const key = "Www-Authenticate"
	for i, v := range h[key] {
		for {
			fixed := missingComma.ReplaceAllString(v, "$1, $2")
			if fixed == v {
				break
			}
			v = fixed
		}
		h[key][i] = v
	}
}

// identity is the part of a backend's declaration that name-keyed state is bound to.
func (b *Backend) identity() config.Identity { return config.IdentityOf(b.spec) }

// Spec is the declaration this backend was built from, which a reload compares against
// the file to decide whether anything actually changed.
func (b *Backend) Spec() config.Backend { return b.spec }
