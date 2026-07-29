// Package backend owns mcpd's upstream MCP sessions: one shared session per
// backend, with at-most-once tool dispatch.
package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

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

var errDisabled = errors.New("backend disabled")

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

// environ is a package var so tests can inject a parent environment.
var environ = os.Environ

const (
	backoffBase = 250 * time.Millisecond
	backoffMax  = 30 * time.Second
	// defaultConnectTimeout bounds a handshake when the backend declares no
	// timeout. Unbounded, a hung spawn would hold the lifecycle mutex forever and
	// a disable could never take it.
	defaultConnectTimeout = 60 * time.Second
)

// Backend is one upstream MCP server, shared by every connected client.
//
// Lock order is gate, then life, then mu. Task 5's disable takes gate
// exclusively before life, so dispatch must never acquire them the other way
// round.
type Backend struct {
	name   string
	spec   config.Backend
	client *mcp.Client
	dial   func(context.Context) (mcp.Transport, error)

	gate sync.RWMutex // RLock is a dispatch lease; Lock closes and drains it
	life sync.Mutex   // serializes lifecycle transitions
	gen  atomic.Uint64

	mu            sync.Mutex
	session       *mcp.ClientSession
	health        Health
	failures      int
	retryAt       time.Time
	connectCancel context.CancelFunc
}

// Generation changes on every lifecycle transition, so an operation that
// started before one can discard its result rather than commit it.
func (b *Backend) Generation() uint64 { return b.gen.Load() }

func (b *Backend) Health() Health {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.health
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
		b.noteErr(err)
		return nil, fmt.Errorf("backend %s: list tools: %w", b.name, err)
	}
	if b.gen.Load() != gen {
		return nil, fmt.Errorf("backend %s: list tools: %w", b.name, ErrStaleGeneration)
	}

	b.mu.Lock()
	b.health.ToolCount = len(res.Tools)
	b.health.LastRefresh = time.Now()
	b.health.LastErr = ""
	b.mu.Unlock()
	return res.Tools, nil
}

func (b *Backend) ensureSession(ctx context.Context) (*mcp.ClientSession, error) {
	b.mu.Lock()
	sess, state := b.session, b.health.State
	b.mu.Unlock()

	if state == StateDisabled {
		return nil, errDisabled
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
		return nil, errDisabled
	}
	if sess := b.session; sess != nil {
		b.mu.Unlock()
		return sess, nil
	}
	if wait := time.Until(b.retryAt); wait > 0 {
		last := b.health.LastErr
		b.mu.Unlock()
		return nil, fmt.Errorf("backing off %s after: %s", wait.Round(time.Millisecond), last)
	}
	ctx, cancel := context.WithTimeout(ctx, b.connectTimeout())
	b.connectCancel = cancel
	b.mu.Unlock()
	defer func() {
		cancel()
		b.mu.Lock()
		b.connectCancel = nil
		b.mu.Unlock()
	}()

	transport, err := b.dial(ctx)
	if err != nil {
		b.noteConnectFailure(err)
		return nil, fmt.Errorf("build transport: %w", err)
	}
	sess, err := b.client.Connect(ctx, transport, nil)
	if err != nil {
		b.noteConnectFailure(err)
		return nil, fmt.Errorf("connect: %w", err)
	}

	b.mu.Lock()
	b.session = sess
	b.failures = 0
	b.retryAt = time.Time{}
	b.health.State = StateUp
	b.health.LastErr = ""
	b.mu.Unlock()
	b.gen.Add(1)

	go b.watch(sess)
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

func (b *Backend) connectTimeout() time.Duration {
	if b.spec.TimeoutSec > 0 {
		return time.Duration(b.spec.TimeoutSec) * time.Second
	}
	return defaultConnectTimeout
}

// closeSession ends the shared session if there is one. Task 5's disable builds
// its teardown on this.
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

func (b *Backend) noteConnectFailure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.health.State = StateDown
	b.health.LastErr = err.Error()
	b.retryAt = time.Now().Add(min(backoffBase<<min(b.failures-1, 8), backoffMax))
}

func (b *Backend) noteErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.health.LastErr = err.Error()
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

func (b *Backend) httpTransport(context.Context) (mcp.Transport, error) {
	return &mcp.StreamableClientTransport{
		Endpoint: b.spec.HTTPURL,
		// No http.Client.Timeout: it would also cap the long-lived standalone SSE
		// stream. Per-call deadlines come from withTimeout instead.
		HTTPClient: &http.Client{Transport: headerTransport{
			base:    http.DefaultTransport,
			headers: b.spec.ExpandHeaders(environ()),
		}},
	}, nil
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
	return t.base.RoundTrip(req)
}
