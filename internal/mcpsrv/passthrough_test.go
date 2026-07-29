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

func TestPassthroughSyncUnadvertisesAToolThatBecomesInvalidThenDropped(t *testing.T) {
	// A live fake cannot hold a non-object schema at all (its own AddTool would
	// panic on SetTools), so a regressed schema is injected the same way
	// TestPassthroughSyncSkipsANonObjectSchemaWithoutPanicking does: directly
	// into the persisted catalog, bypassing any backend round trip.
	cfg := &config.Config{Backends: map[string]config.Backend{
		"odd": {Name: "odd", HTTPURL: deadEndpoint(t)},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})

	catPath := filepath.Join(t.TempDir(), "catalog.json")
	id := catalog.CanonicalID("odd", "good_tool")
	writeCatalogFile(t, catPath, catalog.Entry{ID: id, Server: "odd", Tool: "good_tool", Schema: []byte(`{"type":"object"}`)})
	cat := catalog.New(reg, catPath)
	if err := cat.Load(); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	p := NewPassthrough(cat, reg)
	client := connectClient(t, p.Server())

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if want := []string{"mcp__odd__good_tool"}; !strEqual(toolNames(got.Tools), want) {
		t.Fatalf("passthrough tools = %v, want %v", toolNames(got.Tools), want)
	}

	// Same id, same tool, but the schema regressed to non-object: the entry is
	// still in the catalog, so it is not in the "gone" set, and Sync must
	// actively remove it rather than merely fail to re-add it.
	writeCatalogFile(t, catPath, catalog.Entry{ID: id, Server: "odd", Tool: "good_tool", Schema: []byte(`{"type":"array"}`)})
	if err := cat.Load(); err != nil {
		t.Fatalf("reload catalog: %v", err)
	}
	p.Sync()

	got, err = client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools after invalid schema: %v", err)
	}
	if names := toolNames(got.Tools); len(names) != 0 {
		t.Fatalf("passthrough tools after invalid schema = %v, want none (a skipped id must be unadvertised)", names)
	}

	// The entry is now fully dropped, not merely invalid. Sync must still leave
	// it unadvertised: bookkeeping already forgot it on the previous Sync, so a
	// later RemoveTools(gone...) computed against known would never fire for it.
	cat.Drop("odd")
	p.Sync()

	got, err = client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools after drop: %v", err)
	}
	if names := toolNames(got.Tools); len(names) != 0 {
		t.Fatalf("passthrough tools after drop = %v, want none (the stale tool must not survive cat.Drop)", names)
	}
}

// TestPassthroughSyncUnadvertisesAToolWhoseAddToolPanics covers the other skip
// site: a schema whose top-level "type" is "object" (isObjectSchema accepts
// it) but that still panics in the SDK's AddTool. This is not hypothetical:
// SDK v1.7.0's validateParamHeaderAnnotations rejects "x-mcp-header" on a
// non-primitive property, confirmed directly against mcp.Server.AddTool.
func TestPassthroughSyncUnadvertisesAToolWhoseAddToolPanics(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{
		"odd": {Name: "odd", HTTPURL: deadEndpoint(t)},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})

	catPath := filepath.Join(t.TempDir(), "catalog.json")
	id := catalog.CanonicalID("odd", "good_tool")
	writeCatalogFile(t, catPath, catalog.Entry{ID: id, Server: "odd", Tool: "good_tool", Schema: []byte(`{"type":"object"}`)})
	cat := catalog.New(reg, catPath)
	if err := cat.Load(); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	p := NewPassthrough(cat, reg)
	client := connectClient(t, p.Server())
	if got, err := client.ListTools(t.Context(), nil); err != nil || !strEqual(toolNames(got.Tools), []string{"mcp__odd__good_tool"}) {
		t.Fatalf("setup: tools = %+v, err = %v", got, err)
	}

	badSchema := []byte(`{"type":"object","properties":{"auth":{"type":"object","x-mcp-header":"Authorization"}}}`)
	writeCatalogFile(t, catPath, catalog.Entry{ID: id, Server: "odd", Tool: "good_tool", Schema: badSchema})
	if err := cat.Load(); err != nil {
		t.Fatalf("reload catalog: %v", err)
	}
	p.Sync() // must not panic, and must not leave the old tool advertised

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools after AddTool panic: %v", err)
	}
	if names := toolNames(got.Tools); len(names) != 0 {
		t.Fatalf("passthrough tools after AddTool panic = %v, want none (a skipped id must be unadvertised)", names)
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

// TestPassthroughSyncIsSafeConcurrentWithCatalogCommits stands in for Task
// 11's post-commit hook, which does not exist yet: it hammers cat.Trigger
// (driving real commits, each briefly taking the catalog's own mutex) on one
// set of goroutines while hammering p.Sync on another, so that if Sync's
// mu.Lock()-across-cat.Entries() ever created a lock-ordering cycle with the
// catalog's own mutex, -race or a deadlock would surface it here rather than
// after Task 11 wires a real hook.
func TestPassthroughSyncIsSafeConcurrentWithCatalogCommits(t *testing.T) {
	fakeA := testfake.New("a", tool("tool_a"))
	fakeB := testfake.New("b", tool("tool_b"))
	reg := httpRegistry(t, fakeA, fakeB)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("a"); cat.StopRefresh("b") })

	p := NewPassthrough(cat, reg)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				cat.Trigger("a")
				cat.Trigger("b")
			}
		}()
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				p.Sync()
			}
		}()
	}
	wg.Wait()
	cat.WaitIdle()
	p.Sync()

	client := connectClient(t, p.Server())
	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := []string{"mcp__a__tool_a", "mcp__b__tool_b"}
	if names := toolNames(got.Tools); !strEqual(names, want) {
		t.Fatalf("passthrough tools = %v, want %v", names, want)
	}
}
