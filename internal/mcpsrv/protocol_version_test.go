package mcpsrv

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
)

// A client speaking a newer protocol than a backend must not leak its version
// onto the backend's requests: the backend call has to carry the version that
// backend negotiated. The backend here rejects anything past 2025-11-25, the way
// LiteLLM's MCP gateway does.
func TestPassthroughCallUsesTheBackendsNegotiatedProtocolVersion(t *testing.T) {
	f := testfake.New("legacy", tool("kubectl_logs"))
	sdk := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return f.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Mcp-Protocol-Version"); v > "2025-11-25" {
			http.Error(w, "Bad Request: Unsupported protocol version: "+v, http.StatusBadRequest)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
		f.Close()
	})

	cfg := &config.Config{Backends: map[string]config.Backend{
		"legacy": {Name: "legacy", HTTPURL: upstream.URL, TimeoutSec: 10},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"), testfake.PermissiveDeclarations{})
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("legacy") })

	// Serve mcpd the way the daemon does (stateless streamable HTTP), not in
	// memory: only there does the tool handler run on the downstream request's
	// context, which carries that client's protocol version.
	p := NewPassthrough(cat, reg)
	downstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return p.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(func() {
		downstream.CloseClientConnections()
		downstream.Close()
	})
	client, err := mcp.NewClient(&mcp.Implementation{Name: "mcpsrv-test", Version: "test"}, nil).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: downstream.URL}, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mcp__legacy__kubectl_logs",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("backend call failed: %s", textOf(t, res))
	}
	if text := textOf(t, res); text != "ok:legacy" {
		t.Fatalf("content = %q, want ok:legacy", text)
	}
}
