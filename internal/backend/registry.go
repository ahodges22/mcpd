package backend

import (
	"context"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
)

const clientVersion = "0.1.0"

// Hooks are the refresh triggers the catalog subscribes to. Any field may be
// nil. Each is delivered on its own goroutine, so a hook may call back into the
// daemon without regard for the lock a lifecycle transition is holding.
type Hooks struct {
	// ToolListChanged fires when a backend reports its tool list has changed.
	ToolListChanged func(server string)
	// Reconnected fires when a session is re-established after one was lost. It is
	// a separate trigger because a tool-list-changed notification cannot be
	// delivered while there is no session, so the list may have moved unseen.
	Reconnected func(server string)
}

// Registry owns one Backend per configured backend and routes calls by name.
type Registry struct {
	backends map[string]*Backend
	names    []string
}

// NewRegistry builds a Backend per configured backend. Sessions are opened
// lazily on first use.
func NewRegistry(cfg *config.Config, hooks Hooks) *Registry {
	r := &Registry{
		backends: make(map[string]*Backend, len(cfg.Backends)),
		names:    make([]string, 0, len(cfg.Backends)),
	}
	for name, spec := range cfg.Backends {
		r.backends[name] = newBackend(name, spec, hooks)
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
		b.health.AuthNote = "oauth"
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
