package backend

import (
	"context"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
)

const clientVersion = "0.1.0"

// Registry owns one Backend per configured backend and routes calls by name.
type Registry struct {
	backends map[string]*Backend
	names    []string
}

// NewRegistry builds a Backend per configured backend. Sessions are opened
// lazily on first use. onToolListChanged, if non-nil, fires with the backend's
// name when that backend reports its tool list has changed; the catalog uses it
// as a refresh trigger.
func NewRegistry(cfg *config.Config, onToolListChanged func(server string)) *Registry {
	r := &Registry{
		backends: make(map[string]*Backend, len(cfg.Backends)),
		names:    make([]string, 0, len(cfg.Backends)),
	}
	for name, spec := range cfg.Backends {
		r.backends[name] = newBackend(name, spec, onToolListChanged)
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

func newBackend(name string, spec config.Backend, onToolListChanged func(server string)) *Backend {
	b := &Backend{
		name: name,
		spec: spec,
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
	if onToolListChanged != nil {
		opts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) {
			onToolListChanged(name)
		}
	}
	b.client = mcp.NewClient(&mcp.Implementation{Name: "mcpd", Version: clientVersion}, opts)
	return b
}
