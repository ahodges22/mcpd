package mcpsrv

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
)

// httpRegistry serves each fake over streamable HTTP so a test runs against a
// real *backend.Registry, mirroring internal/catalog's own test helper.
func httpRegistry(t *testing.T, fakes ...*testfake.Fake) *backend.Registry {
	t.Helper()
	cfg := &config.Config{Backends: make(map[string]config.Backend, len(fakes))}
	for _, f := range fakes {
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return f.Server() },
			&mcp.StreamableHTTPOptions{Stateless: true},
		))
		t.Cleanup(func() {
			srv.CloseClientConnections()
			srv.Close()
			f.Close()
		})
		cfg.Backends[f.Name] = config.Backend{Name: f.Name, HTTPURL: srv.URL, TimeoutSec: 10}
	}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"), testfake.PermissiveDeclarations{})
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	return backend.NewRegistry(cfg, ov, backend.Hooks{})
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: name + " description",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}}}`),
	}
}

// deadEndpoint returns a URL on a port that has just been released, so a dial
// fails immediately rather than depending on name resolution.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr + "/mcp"
}

// connectClient dials srv over an in-memory transport, so a test can list and
// call its tools like a real MCP client would.
func connectClient(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverSide, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "mcpsrv-test", Version: "test"}, nil).
		Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	slices.Sort(names)
	return names
}

func strEqual(a, b []string) bool { return slices.Equal(a, b) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func decodeStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("remarshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
}

type searchResultDTO struct {
	ID          string `json:"id"`
	Server      string `json:"server"`
	Description string `json:"description"`
}

type searchOutputDTO struct {
	Results []searchResultDTO `json:"results"`
	Message string            `json:"message,omitempty"`
}

func callSearch(t *testing.T, client *mcp.ClientSession, query string, limit int) searchOutputDTO {
	t.Helper()
	args := map[string]any{"query": query}
	if limit > 0 {
		args["limit"] = limit
	}
	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "search_tools", Arguments: args})
	if err != nil {
		t.Fatalf("call search_tools: %v", err)
	}
	if res.IsError {
		t.Fatalf("search_tools returned an error: %s", textOf(t, res))
	}
	var out searchOutputDTO
	decodeStructured(t, res, &out)
	return out
}

type describeOutputDTO struct {
	ID          string               `json:"id"`
	Server      string               `json:"server"`
	Description string               `json:"description,omitempty"`
	InputSchema any                  `json:"input_schema,omitempty"`
	Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
}

func callDescribe(t *testing.T, client *mcp.ClientSession, id string) describeOutputDTO {
	t.Helper()
	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "describe_tool",
		Arguments: map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("call describe_tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("describe_tool returned an error: %s", textOf(t, res))
	}
	var out describeOutputDTO
	decodeStructured(t, res, &out)
	return out
}
