package backend

import (
	"context"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
)

const clientVersion = "0.1.0"

// Hooks are the catalog operations a backend drives. Any field may be nil.
//
// The two refresh triggers are notifications, delivered without a lifecycle lock
// held so a hook may call back into the daemon freely. The three a lifecycle
// transition calls are synchronous, because a disable that returned while a
// refresh could still commit would not be a kill switch.
type Hooks struct {
	// ToolListChanged fires when a backend reports its tool list has changed.
	ToolListChanged func(server string)
	// Reconnected fires when a session is re-established after one was lost. It is
	// a separate trigger because a tool-list-changed notification cannot be
	// delivered while there is no session, so the list may have moved unseen.
	Reconnected func(server string)
	// StopRefresh must cancel a pending or in-flight refresh and return only once
	// the refresh loop has exited. A disable calls it before closing the session.
	StopRefresh func(server string)
	// DropTools must evict a backend's tools from the catalog. A disable calls it
	// last, once nothing is left that could commit them again.
	DropTools func(server string)
	// Refresh must re-read a backend's tools. An enable calls it so a re-enabled
	// backend's tools reappear without waiting for the TTL tick.
	Refresh func(server string)
	// AuthHandler supplies the handler an HTTP backend declaring auth "oauth"
	// authorizes with. It is called on every dial and must return the same handler
	// for a given backend, because that handler owns the live token source and a
	// reconnect must not re-authorize.
	AuthHandler func(server string) (auth.OAuthHandler, error)
}

// Registry owns one Backend per configured backend and routes calls by name.
type Registry struct {
	backends  map[string]*Backend
	names     []string
	overrides *Overrides
}

// NewRegistry builds a Backend per configured backend. Sessions are opened
// lazily on first use.
func NewRegistry(cfg *config.Config, ov *Overrides, hooks Hooks) *Registry {
	r := &Registry{
		backends:  make(map[string]*Backend, len(cfg.Backends)),
		names:     make([]string, 0, len(cfg.Backends)),
		overrides: ov,
	}
	for name, spec := range cfg.Backends {
		b := newBackend(name, spec, hooks)
		if ov.Disabled(name) {
			b.health.State = StateDisabled
		}
		r.backends[name] = b
		r.names = append(r.names, name)
	}
	slices.Sort(r.names)
	return r
}

func (r *Registry) Get(name string) (*Backend, bool) {
	b, ok := r.backends[name]
	return b, ok
}

func (r *Registry) Names() []string { return slices.Clone(r.names) }

func (r *Registry) Health() map[string]Health {
	out := make(map[string]Health, len(r.backends))
	for name, b := range r.backends {
		out[name] = b.Health()
	}
	return out
}

func newBackend(name string, spec config.Backend, hooks Hooks) *Backend {
	b := &Backend{
		name:        name,
		spec:        spec,
		onReconnect: hooks.Reconnected,
		stopRefresh: hooks.StopRefresh,
		dropTools:   hooks.DropTools,
		refresh:     hooks.Refresh,
		authHandler: hooks.AuthHandler,
		health: Health{
			State:     StateDown,
			Transport: "http",
		},
	}
	if spec.IsStdio() {
		b.health.Transport = "stdio"
		b.dial = b.stdioTransport
	} else {
		b.dial = b.httpTransport
	}
	if spec.Auth == "oauth" {
		b.authNote = "oauth"
		b.health.AuthNote = b.authNote
	}

	// Advertising no sampling, elicitation or roots capability is what keeps
	// server-initiated flows from arriving at all: one shared session cannot
	// attribute them to one of several connected clients.
	opts := &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}}
	if changed := hooks.ToolListChanged; changed != nil {
		opts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) {
			changed(name)
		}
	}
	b.client = mcp.NewClient(&mcp.Implementation{Name: "mcpd", Version: clientVersion}, opts)
	return b
}

// Disable is the kill switch: it closes the dispatch gate, cancels and awaits
// everything in flight, closes the session, terminates a stdio child, and evicts
// the backend's tools from the catalog. The override is persisted before any of
// that begins, so a crash mid-teardown leaves the backend disabled rather than
// silently re-enabled.
func (r *Registry) Disable(name string) error {
	b, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("unknown backend %q", name)
	}
	b.transition.Lock()
	defer b.transition.Unlock()
	if err := r.overrides.set(name, true); err != nil {
		return fmt.Errorf("disable %s: %w", name, err)
	}
	b.teardown(forDisable)
	return nil
}

// Reconnect ends the shared session so the next dispatch or list re-dials, and
// clears the backoff window, without which a backend inside it would ignore the
// user's explicit request. It writes no override, because the user's enable and
// disable intent is unchanged, and it refuses a disabled backend rather than
// resurrecting a child the kill switch killed. The state check is inside the
// transition lock, so it cannot straddle a concurrent Disable.
func (r *Registry) Reconnect(name string) error {
	_, err := r.ReconnectGeneration(name)
	return err
}

// ReconnectGeneration identifies the lifecycle whose refresh this reconnect starts.
func (r *Registry) ReconnectGeneration(name string) (uint64, error) {
	b, ok := r.Get(name)
	if !ok {
		return 0, fmt.Errorf("unknown backend %q", name)
	}
	b.transition.Lock()
	defer b.transition.Unlock()
	if b.Health().State == StateDisabled {
		return 0, fmt.Errorf("reconnect %s: %w", name, ErrDisabled)
	}
	b.teardown(forReconnect)
	generation := b.Generation()
	// A refresh rather than an eviction: the tools stay in the catalog, and this is
	// what re-dials instead of waiting for the next client request.
	if b.refresh != nil {
		b.refresh(name)
	}
	return generation, nil
}

// Enable clears the override and lets the backend connect again on its next use.
// It runs under the same locks as Disable, so a re-enable cannot race a teardown.
func (r *Registry) Enable(name string) error {
	b, ok := r.Get(name)
	if !ok {
		return fmt.Errorf("unknown backend %q", name)
	}
	b.transition.Lock()
	defer b.transition.Unlock()
	if err := r.overrides.set(name, false); err != nil {
		return fmt.Errorf("enable %s: %w", name, err)
	}
	b.restore()
	return nil
}
