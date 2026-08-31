// Package mcpsrv builds the two MCP servers mcpd exposes: a three-tool
// search facade and a full-catalog pass-through, both dispatching through the
// same catalog and backend registry.
package mcpsrv

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/rank"
	"github.com/ahodges22/mcpd/internal/version"
)

const defaultSearchLimit = 10

type SearchRequest struct {
	Query string `json:"query" jsonschema:"the search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results to return, default 10"`
}

type SearchResult struct {
	ID            string `json:"id"`
	Server        string `json:"server"`
	Description   string `json:"description"`
	LowConfidence bool   `json:"low_confidence,omitempty"`
}

type SearchResponse struct {
	Results       []SearchResult `json:"results"`
	Message       string         `json:"message,omitempty"`
	DegradedError string         `json:"degraded_error,omitempty"`
}

type DescribeRequest struct {
	ID string `json:"id" jsonschema:"the canonical tool id, e.g. mcp__github__create_pull_request"`
}

type DescribeResponse struct {
	ID          string               `json:"id"`
	Server      string               `json:"server"`
	Description string               `json:"description,omitempty"`
	InputSchema any                  `json:"input_schema,omitempty"`
	Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
}

type CallRequest struct {
	ID        string         `json:"id" jsonschema:"the canonical tool id, e.g. mcp__github__create_pull_request"`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"the tool's arguments"`
}

// NewSearch builds the three-tool search facade: search_tools ranks the
// catalog, describe_tool serves a schema from the catalog with no upstream
// call, and call_tool dispatches to the owning backend.
// SearchIndex supplies the shared production/eval ranking pipeline. It is optional so a
// daemon with no embeddings gateway still degrades to lexical ranking.
type SearchIndex interface {
	Search(context.Context, string, []catalog.Entry, int) ([]rank.Result, rank.Evidence, error)
}

func NewSearch(cat *catalog.Catalog, reg *backend.Registry, th rank.Thresholds, index SearchIndex) *mcp.Server {
	ops := NewOperations(cat, reg, th, index)
	s := mcp.NewServer(&mcp.Implementation{Name: "mcpd-search", Version: version.String()},
		&mcp.ServerOptions{Instructions: searchInstructions(reg)})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_tools",
		Description: "Search the catalog of every tool every connected backend offers.",
	}, searchHandler(ops))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "describe_tool",
		Description: "Fetch a tool's full input schema from the catalog. Never contacts the backend.",
	}, describeHandler(ops))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "call_tool",
		Description: "Call a tool found by search_tools or describe_tool, identified by its canonical id.",
	}, callHandler(ops))

	return s
}

type Operations struct {
	cat        *catalog.Catalog
	reg        *backend.Registry
	thresholds rank.Thresholds
	index      SearchIndex
}

func NewOperations(cat *catalog.Catalog, reg *backend.Registry, th rank.Thresholds, index SearchIndex) *Operations {
	return &Operations{cat: cat, reg: reg, thresholds: th, index: index}
}

func (o *Operations) Search(ctx context.Context, in SearchRequest) SearchResponse {
	cat, reg, th, index := o.cat, o.reg, o.thresholds, o.index
	entries := cat.Entries()
	if len(entries) == 0 {
		return SearchResponse{Message: explainEmptyCatalog(cat, reg)}
	}

	limit := in.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	var fused []rank.Result
	var evidence rank.Evidence
	var degraded string
	if index == nil {
		fused, evidence = rank.Fuse(in.Query, entries, nil, nil, limit)
	} else {
		var err error
		fused, evidence, err = index.Search(ctx, in.Query, entries, limit)
		if err != nil {
			slog.Warn("hybrid ranking degraded to fusion", "error", err)
			degraded = err.Error()
		}
	}
	if len(fused) == 0 {
		return SearchResponse{Message: fmt.Sprintf("no tools match your query %q", in.Query), DegradedError: degraded}
	}

	low := th.LowConfidence(evidence)
	out := SearchResponse{Results: make([]SearchResult, len(fused)), DegradedError: degraded}
	for i, r := range fused {
		out.Results[i] = SearchResult{ID: r.ID, Server: r.Server, Description: r.Description, LowConfidence: low}
	}
	if low {
		out.Message = "low confidence: these results may not answer the query"
	}
	return out
}

func searchHandler(o *Operations) mcp.ToolHandlerFor[SearchRequest, SearchResponse] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchRequest) (*mcp.CallToolResult, SearchResponse, error) {
		return nil, o.Search(ctx, in), nil
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

func (o *Operations) Describe(id string) (DescribeResponse, error) {
	entry, ok := o.cat.Lookup(id)
	if !ok {
		return DescribeResponse{}, fmt.Errorf("unknown tool id %q", id)
	}
	return DescribeResponse{
		ID:          entry.ID,
		Server:      entry.Server,
		Description: entry.Description,
		InputSchema: entry.Schema,
		Annotations: entry.Annotations,
	}, nil
}

func describeHandler(o *Operations) mcp.ToolHandlerFor[DescribeRequest, DescribeResponse] {
	return func(_ context.Context, _ *mcp.CallToolRequest, in DescribeRequest) (*mcp.CallToolResult, DescribeResponse, error) {
		out, err := o.Describe(in.ID)
		return nil, out, err
	}
}

func (o *Operations) Call(ctx context.Context, in CallRequest) (*mcp.CallToolResult, error) {
	entry, ok := o.cat.Lookup(in.ID)
	if !ok {
		return nil, fmt.Errorf("unknown tool id %q", in.ID)
	}
	b, ok := o.reg.Get(entry.Server)
	if !ok {
		return nil, fmt.Errorf("tool %s: backend %q not connected", in.ID, entry.Server)
	}
	res, err := b.Call(ctx, entry.Tool, in.Arguments)
	if err != nil {
		// err distinguishes backend.ErrNotAttempted (safe to retry) from every
		// other outcome (unknown, never retried). Forwarded verbatim: collapsing
		// the two into one message would erase the only signal the caller has for
		// deciding whether it is safe to try again.
		return nil, err
	}
	return res, nil
}

func callHandler(o *Operations) mcp.ToolHandlerFor[CallRequest, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in CallRequest) (*mcp.CallToolResult, any, error) {
		res, err := o.Call(ctx, in)
		return res, nil, err
	}
}
