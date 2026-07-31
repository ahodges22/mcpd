package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

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

// ErrRegistryShutdown reports that the daemon is shutting down, so nothing new may
// be published. Callers check it before mutating anything, because a refusal after a
// declaration was committed would leave the file and the registry disagreeing.
var ErrRegistryShutdown = errors.New("registry shut down")

// Registry owns one Backend per configured backend and routes calls by name.
//
// The map and the name list are mutable at run time, so both are guarded by mu. mu is
// never held while any backend lock is acquired, which is what lets a reload
// replacement hold one backend's transition lock across its own map mutation.
type Registry struct {
	mu        sync.RWMutex
	backends  map[string]*Backend
	names     []string
	shutdown  bool
	hooks     Hooks
	overrides *Overrides
}

// NewRegistry builds a Backend per configured backend. Sessions are opened
// lazily on first use.
func NewRegistry(cfg *config.Config, ov *Overrides, hooks Hooks) *Registry {
	r := &Registry{
		backends:  make(map[string]*Backend, len(cfg.Backends)),
		names:     make([]string, 0, len(cfg.Backends)),
		hooks:     hooks,
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

// Add publishes a backend at run time. It takes the initial enabled state from its
// caller and never consults the override store, unlike construction above: the status
// surface supplies enabled, so a stale disabled entry cannot affect a freshly declared
// backend, and a reload replacement supplies the state it captured, so a declaration
// edit cannot silently enable a backend the user stopped. Reading the store here would
// satisfy the second case and break the first.
//
// It cannot fail, which is what keeps the declaration write the only commit point.
func (r *Registry) Add(name string, spec config.Backend, enabled bool) {
	b := newBackend(name, spec, r.hooks)
	if !enabled {
		b.health.State = StateDisabled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A backend published after shutdown is born latched rather than refused, which
	// keeps Add infallible while still making it impossible to spawn a child that would
	// outlive the daemon. Callers check ShuttingDown before their commit point; this is
	// what covers one that lost the race. No lock is taken on b: it is unpublished, so
	// nothing else can hold a reference to it yet.
	if r.shutdown {
		b.shutdown = true
	}
	if _, exists := r.backends[name]; !exists {
		r.names = append(r.names, name)
		slices.Sort(r.names)
	}
	r.backends[name] = b
}

// Remove unpublishes a backend and then tears it down. The map entry goes first and mu
// is released before the teardown, because a teardown blocks on in-flight work and mu
// must never be held across one.
//
// The teardown is terminal, so nothing can respawn a child for a backend that is no
// longer declared. The latch lives on the object rather than the name, so a later Add
// of the same name builds a fresh backend and is unaffected.
func (r *Registry) Remove(name string) {
	b, ok := r.unpublish(name)
	if !ok {
		return
	}
	b.transition.Lock()
	defer b.transition.Unlock()
	b.teardown(forShutdown)
}

// RemoveHeld is Remove for a caller already holding b's transition lock, which a reload
// replacement does so no enable or disable can land mid-swap.
func (r *Registry) RemoveHeld(b *Backend) {
	r.unpublish(b.name)
	b.teardown(forShutdown)
}

func (r *Registry) unpublish(name string) (*Backend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[name]
	if !ok {
		return nil, false
	}
	delete(r.backends, name)
	r.names = slices.DeleteFunc(r.names, func(n string) bool { return n == name })
	return b, true
}

// ShuttingDown reports whether shutdown has latched.
func (r *Registry) ShuttingDown() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shutdown
}

func (r *Registry) Get(name string) (*Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[name]
	return b, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.names)
}

func (r *Registry) Health() map[string]Health {
	out := make(map[string]Health)
	for name, b := range r.snapshot() {
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
	b, done, err := r.beginTransition(name)
	if err != nil {
		return err
	}
	defer done()
	if err := r.overrides.set(name, true, b.identity()); err != nil {
		return fmt.Errorf("disable %s: %w", name, err)
	}
	b.teardown(forDisable)
	return nil
}

// Shutdown tears every backend down for process exit: it drains the dispatch
// gate, stops each refresh, closes each session and terminates each stdio child.
// It writes no override, because a restart must not find every backend disabled,
// and it evicts no tools, because the persisted catalog is what answers a search
// before the next start has re-listed anything.
//
// It is terminal: the latch it leaves is never cleared, a backend already disabled
// stays disabled, every later transition refuses, and a dial refuses even if one is
// somehow reached. That matters because the web surface deliberately leaves a
// transition running after its response has timed out, so a reconnect-all can still
// be in flight with nothing left to join it, and a respawned child would outlive the
// daemon.
//
// It takes no context and cannot: draining the gate has no cancellable variant, so a
// tools/call with no configured timeout blocks exit until the client gives up.
func (r *Registry) Shutdown() {
	// The latch and the snapshot are taken together, so a backend published after this
	// point cannot slip past the walk below and leave a stdio child alive after exit.
	// The per-backend latch does not cover that case: the dangerous backend is one that
	// was never in the map when shutdown read it.
	r.mu.Lock()
	r.shutdown = true
	backends := make([]*Backend, 0, len(r.names))
	for _, name := range r.names {
		backends = append(backends, r.backends[name])
	}
	r.mu.Unlock()

	for _, b := range backends {
		b.transition.Lock()
		b.teardown(forShutdown)
		b.transition.Unlock()
	}
}

func (r *Registry) snapshot() map[string]*Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*Backend, len(r.backends))
	for name, b := range r.backends {
		out[name] = b
	}
	return out
}

// beginTransition takes name's transition lock, and refuses once a shutdown has
// latched the backend. Every user-initiated transition starts here, so none of them
// can undo a shutdown: an enable or a reconnect triggers a refresh, and a refresh
// after a shutdown both re-dials and evicts the tools the shutdown kept.
func (r *Registry) beginTransition(name string) (*Backend, func(), error) {
	b, ok := r.Get(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown backend %q", name)
	}
	b.transition.Lock()
	if b.shuttingDown() {
		b.transition.Unlock()
		return nil, nil, fmt.Errorf("%s: %w", name, ErrShutdown)
	}
	return b, b.transition.Unlock, nil
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
	b, done, err := r.beginTransition(name)
	if err != nil {
		return 0, err
	}
	defer done()
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
	b, done, err := r.beginTransition(name)
	if err != nil {
		return err
	}
	defer done()
	if err := r.overrides.set(name, false, b.identity()); err != nil {
		return fmt.Errorf("enable %s: %w", name, err)
	}
	b.restore()
	return nil
}
