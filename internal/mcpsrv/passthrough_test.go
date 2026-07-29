package mcpsrv

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/testfake"
)

func TestPassthroughAdvertisesCanonicalNames(t *testing.T) {
	reg := httpRegistry(t,
		testfake.New("github", tool("create_pull_request")),
		testfake.New("infra", tool("kubectl_logs")),
	)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github"); cat.StopRefresh("infra") })

	p := NewPassthrough(cat, reg)
	client := connectClient(t, p.Server())

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := []string{"mcp__github__create_pull_request", "mcp__infra__kubectl_logs"}
	if names := toolNames(got.Tools); !strEqual(names, want) {
		t.Fatalf("passthrough tools = %v, want %v", names, want)
	}
}

func TestPassthroughCallReachesTheOwningBackend(t *testing.T) {
	reg := httpRegistry(t,
		testfake.New("github", tool("create_pull_request")),
		testfake.New("infra", tool("kubectl_logs")),
	)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github"); cat.StopRefresh("infra") })

	p := NewPassthrough(cat, reg)
	client := connectClient(t, p.Server())

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "mcp__infra__kubectl_logs",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	if text := textOf(t, res); text != "ok:infra" {
		t.Fatalf("content = %q, want ok:infra (the owning backend must answer)", text)
	}
}

func TestPassthroughSyncNotifiesAndReconcilesOnCatalogChange(t *testing.T) {
	fake := testfake.New("github", tool("create_pull_request"))
	reg := httpRegistry(t, fake)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })

	p := NewPassthrough(cat, reg)

	var notified atomic.Int64
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := p.Server().Connect(context.Background(), serverSide, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	opts := &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
		notified.Add(1)
	}}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "mcpsrv-test", Version: "test"}, opts).
		Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if want := []string{"mcp__github__create_pull_request"}; !strEqual(toolNames(got.Tools), want) {
		t.Fatalf("passthrough tools = %v, want %v", toolNames(got.Tools), want)
	}

	fake.SetTools(tool("close_issue"))
	cat.RefreshAll(t.Context())
	p.Sync()

	waitFor(t, func() bool { return notified.Load() > 0 })

	got, err = client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools after change: %v", err)
	}
	want := []string{"mcp__github__close_issue"}
	if names := toolNames(got.Tools); !strEqual(names, want) {
		t.Fatalf("passthrough tools after change = %v, want %v", names, want)
	}
}

func TestPassthroughSyncSkipsANonObjectSchemaWithoutPanicking(t *testing.T) {
	// catalog.Load discards entries for servers Names() does not list, so "odd"
	// must be configured even though Sync never dials it.
	cfg := &config.Config{Backends: map[string]config.Backend{
		"odd": {Name: "odd", HTTPURL: deadEndpoint(t)},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})

	catPath := filepath.Join(t.TempDir(), "catalog.json")
	writeCatalogFile(t, catPath,
		catalog.Entry{
			ID:     catalog.CanonicalID("odd", "good_tool"),
			Server: "odd",
			Tool:   "good_tool",
			Schema: []byte(`{"type":"object"}`),
		},
		catalog.Entry{
			ID:     catalog.CanonicalID("odd", "bad_tool"),
			Server: "odd",
			Tool:   "bad_tool",
			Schema: []byte(`{"type":"array"}`),
		},
	)
	cat := catalog.New(reg, catPath)
	if err := cat.Load(); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	p := NewPassthrough(cat, reg) // must not panic on the malformed schema
	client := connectClient(t, p.Server())

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := []string{"mcp__odd__good_tool"}
	if names := toolNames(got.Tools); !strEqual(names, want) {
		t.Fatalf("passthrough tools = %v, want %v (the malformed one must be skipped, not crash)", names, want)
	}
}

func TestPassthroughSyncIsSafeForConcurrentCallers(t *testing.T) {
	reg := httpRegistry(t, testfake.New("github", tool("create_pull_request")))
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })

	p := NewPassthrough(cat, reg)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Sync()
		}()
	}
	wg.Wait()

	client := connectClient(t, p.Server())
	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if want := []string{"mcp__github__create_pull_request"}; !strEqual(toolNames(got.Tools), want) {
		t.Fatalf("passthrough tools = %v, want %v", toolNames(got.Tools), want)
	}
}
