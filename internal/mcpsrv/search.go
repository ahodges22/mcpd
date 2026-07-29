// Package mcpsrv builds the two MCP servers mcpd exposes: a three-tool
// search facade and a full-catalog pass-through, both dispatching through the
// same catalog and backend registry.
package mcpsrv

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/rank"
)

const defaultSearchLimit = 10

type searchInput struct {
	Query string `json:"query" jsonschema:"the search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results to return, default 10"`
}

type searchResult struct {
	ID            string `json:"id"`
	Server        string `json:"server"`
	Description   string `json:"description"`
	LowConfidence bool   `json:"low_confidence,omitempty"`
}

type searchOutput struct {
	Results []searchResult `json:"results"`
	Message string         `json:"message,omitempty"`
}

type describeInput struct {
	ID string `json:"id" jsonschema:"the canonical tool id, e.g. mcp__github__create_pull_request"`
}

type describeOutput struct {
	ID          string               `json:"id"`
	Server      string               `json:"server"`
	Description string               `json:"description,omitempty"`
	InputSchema any                  `json:"input_schema,omitempty"`
	Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
}

type callInput struct {
	ID        string         `json:"id" jsonschema:"the canonical tool id, e.g. mcp__github__create_pull_request"`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"the tool's arguments"`
}

// NewSearch builds the three-tool search facade: search_tools ranks the
// catalog, describe_tool serves a schema from the catalog with no upstream
// call, and call_tool dispatches to the owning backend.
func NewSearch(cat *catalog.Catalog, reg *backend.Registry, th rank.Thresholds) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcpd-search", Version: "dev"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_tools",
		Description: "Search the catalog of every tool every connected backend offers.",
	}, searchHandler(cat, reg, th))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "describe_tool",
		Description: "Fetch a tool's full input schema from the catalog. Never contacts the backend.",
	}, describeHandler(cat))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "call_tool",
		Description: "Call a tool found by search_tools or describe_tool, identified by its canonical id.",
	}, callHandler(cat, reg))

	return s
}

func searchHandler(cat *catalog.Catalog, reg *backend.Registry, th rank.Thresholds) mcp.ToolHandlerFor[searchInput, searchOutput] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
		entries := cat.Entries()
		if len(entries) == 0 {
			return nil, searchOutput{Message: explainEmptyCatalog(cat, reg)}, nil
		}

		limit := in.Limit
		if limit <= 0 {
			limit = defaultSearchLimit
		}
		fused, evidence := rank.Fuse(in.Query, entries, nil, nil, limit)
		if len(fused) == 0 {
			return nil, searchOutput{Message: fmt.Sprintf("no tools match your query %q", in.Query)}, nil
		}

		low := th.LowConfidence(evidence)
		out := searchOutput{Results: make([]searchResult, len(fused))}
		for i, r := range fused {
			out.Results[i] = searchResult{ID: r.ID, Server: r.Server, Description: r.Description, LowConfidence: low}
		}
		if low {
			out.Message = "low confidence: these results may not answer the query"
		}
		return nil, out, nil
	}
}

// explainEmptyCatalog names, per backend, why the catalog has nothing to
// search: the model needs to know whether to wait for a backend to connect,
// give up because nothing is configured, or stop expecting tools from a
// backend that connected but reported none, rather than always retrying.
//
// Errors() being empty does not mean every backend is fine: commit deletes a
// server's error record on any successful refresh, including one that found
// zero tools, and a backend that has never been refreshed yet has no error
// either. Both are reachable and neither means "not connected", so this
// checks each configured backend's own health rather than asserting one
// blanket cause.
func explainEmptyCatalog(cat *catalog.Catalog, reg *backend.Registry) string {
	errs := cat.Errors()
	if len(errs) > 0 {
		names := make([]string, 0, len(errs))
		for name := range errs {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = name + ": " + errs[name]
		}
		return "no tools are available: " + strings.Join(parts, "; ")
	}

	names := reg.Names()
	if len(names) == 0 {
		return "no tools are available: no backends are configured"
	}
	health := reg.Health()
	for _, name := range names {
		if health[name].State == backend.StateUp {
			return "no tools are available: backends are connected but none reported any tools"
		}
	}
	return "no tools are available: backends are configured but have not connected yet"
}

func describeHandler(cat *catalog.Catalog) mcp.ToolHandlerFor[describeInput, describeOutput] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, describeOutput, error) {
		entry, ok := cat.Lookup(in.ID)
		if !ok {
			return nil, describeOutput{}, fmt.Errorf("unknown tool id %q", in.ID)
		}
		return nil, describeOutput{
			ID:          entry.ID,
			Server:      entry.Server,
			Description: entry.Description,
			InputSchema: entry.Schema,
			Annotations: entry.Annotations,
		}, nil
	}
}

func callHandler(cat *catalog.Catalog, reg *backend.Registry) mcp.ToolHandlerFor[callInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in callInput) (*mcp.CallToolResult, any, error) {
		entry, ok := cat.Lookup(in.ID)
		if !ok {
			return nil, nil, fmt.Errorf("unknown tool id %q", in.ID)
		}
		b, ok := reg.Get(entry.Server)
		if !ok {
			return nil, nil, fmt.Errorf("tool %s: backend %q not connected", in.ID, entry.Server)
		}
		res, err := b.Call(ctx, entry.Tool, in.Arguments)
		if err != nil {
			// err distinguishes backend.ErrNotAttempted (safe to retry) from every
			// other outcome (unknown, never retried). Forwarded verbatim: collapsing
			// the two into one message would erase the only signal the caller has for
			// deciding whether it is safe to try again.
			return nil, nil, err
		}
		return res, nil, nil
	}
}
