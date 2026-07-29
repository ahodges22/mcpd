// Package testfake provides an in-process MCP backend for mcpd's tests.
package testfake

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var objectSchema = json.RawMessage(`{"type":"object"}`)

// Fake is an in-process MCP server. Tests assert on its counters and its
// received-request log rather than on a caller's return value, because a caller
// cannot tell a rejected request from a completed one.
//
// Set the hooks before the first Transport call.
type Fake struct {
	Name string

	ListCalls   atomic.Int64 // tools/list requests served
	SideEffects atomic.Int64 // tools/call handler bodies entered
	Dials       atomic.Int64 // sessions opened

	// BeforeList runs before a tools/list is served, so a test can hold one in
	// flight.
	BeforeList func()
	// OnCall runs inside a tools/call handler once its side effect is committed.
	OnCall func(ctx context.Context, req *mcp.CallToolRequest)

	srv *mcp.Server

	mu       sync.Mutex
	tools    []*mcp.Tool
	sessions []*mcp.ServerSession
	received []string
}

// New returns a fake serving tools, each of which counts a side effect and
// replies "ok:<name>" so a test can tell which backend answered.
func New(name string, tools ...*mcp.Tool) *Fake {
	f := &Fake{Name: name}
	f.srv = mcp.NewServer(&mcp.Implementation{Name: name, Version: "test"}, nil)
	f.srv.AddReceivingMiddleware(f.record)
	f.SetTools(tools...)
	return f
}

// Server exposes the underlying MCP server, so a test can serve the fake over a
// real transport or add its own middleware.
func (f *Fake) Server() *mcp.Server { return f.srv }

// SetTools replaces the served tool set. The SDK notifies connected sessions,
// so this doubles as the trigger for a tool-list-changed refresh.
func (f *Fake) SetTools(tools ...*mcp.Tool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var gone []string
	for _, old := range f.tools {
		if !hasName(tools, old.Name) {
			gone = append(gone, old.Name)
		}
	}
	f.tools = tools
	if len(gone) > 0 {
		f.srv.RemoveTools(gone...)
	}
	for _, t := range tools {
		add := *t
		if add.InputSchema == nil {
			add.InputSchema = objectSchema
		}
		f.srv.AddTool(&add, f.callTool)
	}
}

// Transport opens a new in-process session and returns the client half. Its
// signature matches the transport constructor a backend dials through.
func (f *Fake) Transport(ctx context.Context) (mcp.Transport, error) {
	serverSide, clientSide := mcp.NewInMemoryTransports()
	ss, err := f.srv.Connect(ctx, serverSide, nil)
	if err != nil {
		return nil, err
	}
	f.Dials.Add(1)
	f.mu.Lock()
	f.sessions = append(f.sessions, ss)
	f.mu.Unlock()
	return clientSide, nil
}

// Close ends every session this fake has served.
func (f *Fake) Close() {
	f.mu.Lock()
	sessions := f.sessions
	f.sessions = nil
	f.mu.Unlock()
	for _, ss := range sessions {
		ss.Close()
	}
}

func (f *Fake) callTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	f.SideEffects.Add(1)
	if f.OnCall != nil {
		f.OnCall(ctx, req)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + f.Name}}}, nil
}

// Received reports every request this fake has received, in order, as its method
// name with the tool name appended for a tools/call. Notifications are not
// requests and are omitted.
func (f *Fake) Received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.received)
}

func (f *Fake) record(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if !strings.HasPrefix(method, "notifications/") {
			name := method
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				name += ":" + p.Name
			}
			f.mu.Lock()
			f.received = append(f.received, name)
			f.mu.Unlock()
		}
		if method == "tools/list" {
			f.ListCalls.Add(1)
			if f.BeforeList != nil {
				f.BeforeList()
			}
		}
		return next(ctx, method, req)
	}
}

func hasName(tools []*mcp.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
