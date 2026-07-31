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
		cat: cat,
		reg: reg,
		srv: mcp.NewServer(&mcp.Implementation{Name: "mcpd-passthrough", Version: "dev"},
			&mcp.ServerOptions{Instructions: passthroughInstructions(reg)}),
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
// is not something this daemon controls or has validated in advance. A
// skipped id is also actively removed if the server was still advertising it
// under a prior, valid version: once this pass forgets an id, no later pass
// will ever call RemoveTools for it, since removal is computed only against
// what this method itself last remembered.
//
// Sync takes only its own mutex, and reaches the catalog exclusively through
// Entries (which takes and releases the catalog's mutex internally, never
// held concurrently with p.mu by any other path here). Sync is therefore safe
// to call from any goroutine, including one invoked synchronously by a
// catalog post-commit hook, as long as the hook fires after the catalog has
// released its own mutex for that commit (the same ordering catalog.commit
// already uses before calling persist) -- never from inside the commit's
// critical section, since Entries would then observe a partial commit rather
// than the one that triggered the hook.
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
			// The id is about to drop out of known below, so if it was advertised
			// under a prior, valid schema, it must be actively removed here: once
			// known forgets it, nothing else will ever call RemoveTools for it.
			p.srv.RemoveTools(id)
			continue
		}
		t := &mcp.Tool{Name: id, Description: e.Description, InputSchema: schema, Annotations: e.Annotations}
		if !addToolSafely(p.srv, t, passthroughCallHandler(p.reg, e.Server, e.Tool)) {
			p.srv.RemoveTools(id)
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
// above. This is not a hypothetical: SDK v1.7.0's AddTool also panics on a
// schema whose top-level type is "object" (isObjectSchema accepts it) if any
// property sets "x-mcp-header" on a non-primitive type (mcp/streamable_headers.go's
// validateParamHeaderAnnotations), which an upstream tool's schema is free to
// do. This recover keeps one malformed upstream tool from taking the whole
// daemon down.
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
