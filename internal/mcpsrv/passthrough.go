package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
)

var objectSchema = json.RawMessage(`{"type":"object"}`)

// Passthrough advertises every catalog tool under its canonical id, and
// dispatches a call to the backend that owns it. Its tool set tracks the
// catalog only when Sync is called; nothing here watches the catalog on its
// own.
type Passthrough struct {
	cat *catalog.Catalog
	reg *backend.Registry
	srv *mcp.Server

	mu    sync.Mutex // serializes Sync against itself, so concurrent callers cannot race the known map
	known map[string]catalog.Entry
}

// NewPassthrough builds the pass-through server and populates it from cat's
// current entries.
func NewPassthrough(cat *catalog.Catalog, reg *backend.Registry) *Passthrough {
	p := &Passthrough{
		cat:   cat,
		reg:   reg,
		srv:   mcp.NewServer(&mcp.Implementation{Name: "mcpd-passthrough", Version: "dev"}, nil),
		known: make(map[string]catalog.Entry),
	}
	p.Sync()
	return p
}

// Server exposes the underlying MCP server, so a caller can serve it over a
// real transport.
func (p *Passthrough) Server() *mcp.Server { return p.srv }

// Sync reconciles the server's advertised tools against the catalog's
// current entries: gone ids are removed, new or changed ones are (re-)added,
// and unchanged ones are left alone so a caller can call Sync freely without
// spamming tool-list-changed notifications for tools that have not moved.
//
// A schema whose top-level "type" is not "object" is skipped rather than
// advertised: the SDK panics on such a schema, and an upstream tool's schema
// is not something this daemon controls or has validated in advance.
func (p *Passthrough) Sync() {
	p.mu.Lock()
	defer p.mu.Unlock()

	entries := p.cat.Entries()
	current := make(map[string]catalog.Entry, len(entries))
	for _, e := range entries {
		current[e.ID] = e
	}

	var gone []string
	for id := range p.known {
		if _, ok := current[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		p.srv.RemoveTools(gone...)
	}

	added := make(map[string]catalog.Entry, len(current))
	for id, e := range current {
		if old, ok := p.known[id]; ok && entryUnchanged(old, e) {
			added[id] = e
			continue
		}
		schema := e.Schema
		if len(schema) == 0 {
			schema = objectSchema
		}
		if !isObjectSchema(schema) {
			slog.Warn("passthrough: refusing to advertise a tool with a non-object input schema", "id", id)
			continue
		}
		t := &mcp.Tool{Name: id, Description: e.Description, InputSchema: schema, Annotations: e.Annotations}
		if !addToolSafely(p.srv, t, passthroughCallHandler(p.reg, e.Server, e.Tool)) {
			continue
		}
		added[id] = e
	}
	p.known = added
}

func entryUnchanged(a, b catalog.Entry) bool {
	return a.Description == b.Description &&
		bytes.Equal(a.Schema, b.Schema) &&
		reflect.DeepEqual(a.Annotations, b.Annotations)
}

func isObjectSchema(schema json.RawMessage) bool {
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		return false
	}
	t, _ := m["type"].(string)
	return t == "object"
}

// addToolSafely guards against any AddTool panic beyond the schema-type check
// above (for example a future SDK validation this daemon has not seen yet),
// so one malformed upstream tool cannot take the whole daemon down.
func addToolSafely(srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandler) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("passthrough: refusing to advertise a tool", "id", t.Name, "panic", r)
			ok = false
		}
	}()
	srv.AddTool(t, h)
	return true
}

func passthroughCallHandler(reg *backend.Registry, server, tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				var res mcp.CallToolResult
				res.SetError(fmt.Errorf("tool %s: invalid arguments: %w", tool, err))
				return &res, nil
			}
		}
		b, ok := reg.Get(server)
		if !ok {
			var res mcp.CallToolResult
			res.SetError(fmt.Errorf("tool %s: backend %q not connected", tool, server))
			return &res, nil
		}
		res, err := b.Call(ctx, tool, args)
		if err != nil {
			// Forwarded verbatim, same as the facade's call_tool: this is the only
			// signal distinguishing a safe-to-retry send from an unknown outcome.
			var errRes mcp.CallToolResult
			errRes.SetError(err)
			return &errRes, nil
		}
		return res, nil
	}
}
