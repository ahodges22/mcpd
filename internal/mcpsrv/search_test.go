package mcpsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/rank"
	"github.com/ahodges/mcpd/internal/testfake"
)

func TestSearchFacadeAdvertisesExactlyThreeTools(t *testing.T) {
	reg := httpRegistry(t)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := toolNames(got.Tools)
	want := []string{"call_tool", "describe_tool", "search_tools"}
	if !strEqual(names, want) {
		t.Fatalf("facade tools = %v, want %v", names, want)
	}
}

func TestSearchToolsExplainsEmptyCatalogWhenNoBackends(t *testing.T) {
	reg := httpRegistry(t)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	out := callSearch(t, client, "anything", 0)
	if len(out.Results) != 0 {
		t.Fatalf("results = %v, want none", out.Results)
	}
	if out.Message == "" {
		t.Fatal("expected an explanatory message for an empty catalog, got none")
	}
	if strings.Contains(out.Message, "no tools match") {
		t.Fatalf("message %q should explain the empty catalog, not a query mismatch", out.Message)
	}
}

func TestSearchToolsExplainsNoMatchAgainstNonEmptyCatalog(t *testing.T) {
	reg := httpRegistry(t, testfake.New("github", tool("create_pull_request")))
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	out := callSearch(t, client, "zzzznomatchzzzz", 0)
	if len(out.Results) != 0 {
		t.Fatalf("results = %v, want none", out.Results)
	}
	if out.Message == "" {
		t.Fatal("expected an explanatory message when nothing matches, got none")
	}
	if strings.Contains(out.Message, "no backends") {
		t.Fatalf("message %q should explain the query mismatch, not an empty catalog", out.Message)
	}
}

func TestSearchToolsReturnsFusedResultsForAMatchingQuery(t *testing.T) {
	reg := httpRegistry(t, testfake.New("github", tool("create_pull_request")))
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	out := callSearch(t, client, "create pull request", 0)
	if len(out.Results) != 1 || out.Results[0].ID != "mcp__github__create_pull_request" {
		t.Fatalf("results = %+v, want the one matching tool", out.Results)
	}
}

func TestDescribeToolServesSchemaWithNoUpstreamCall(t *testing.T) {
	fake := testfake.New("github", tool("create_pull_request"))
	reg := httpRegistry(t, fake)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })

	before := len(fake.Received())

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))
	out := callDescribe(t, client, "mcp__github__create_pull_request")

	if out.Description == "" {
		t.Fatal("expected a description from the catalog")
	}
	if out.InputSchema == nil {
		t.Fatal("expected an input schema from the catalog")
	}
	if got := len(fake.Received()); got != before {
		t.Fatalf("backend received %d new requests, want 0: %v", got-before, fake.Received())
	}
}

func TestDescribeToolUnknownID(t *testing.T) {
	reg := httpRegistry(t)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "describe_tool",
		Arguments: map[string]any{"id": "mcp__nope__nope"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown id")
	}
}

func TestCallToolReachesTheOwningBackend(t *testing.T) {
	reg := httpRegistry(t,
		testfake.New("github", tool("create_pull_request")),
		testfake.New("infra", tool("kubectl_logs")),
	)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github"); cat.StopRefresh("infra") })

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_tool",
		Arguments: map[string]any{"id": "mcp__infra__kubectl_logs", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res.Content)
	}
	text := textOf(t, res)
	if text != "ok:infra" {
		t.Fatalf("content = %q, want ok:infra (the owning backend must answer)", text)
	}
}

func TestCallToolSurfacesNotAttemptedDistinctlyFromOutcomeUnknown(t *testing.T) {
	cfg := &config.Config{Backends: map[string]config.Backend{
		"dead": {Name: "dead", HTTPURL: deadEndpoint(t), TimeoutSec: 1},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})

	// The backend never answers, so the catalog can only learn of its tool
	// from a previously persisted listing, exactly as a restart would load
	// one. This keeps the failure deterministic: the dial itself must fail,
	// rather than racing a live session's teardown.
	catPath := filepath.Join(t.TempDir(), "catalog.json")
	writeCatalogFile(t, catPath, catalog.Entry{
		ID:     catalog.CanonicalID("dead", "some_tool"),
		Server: "dead",
		Tool:   "some_tool",
		Schema: json.RawMessage(`{"type":"object"}`),
	})
	cat := catalog.New(reg, catPath)
	if err := cat.Load(); err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_tool",
		Arguments: map[string]any{"id": "mcp__dead__some_tool", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result against a dead endpoint")
	}
	text := textOf(t, res)
	if !strings.Contains(text, "no send attempted") {
		t.Fatalf("message %q does not say no send was attempted", text)
	}
	if !strings.Contains(text, "some_tool") {
		t.Fatalf("message %q does not name the tool", text)
	}
	if strings.Contains(text, "outcome unknown") {
		t.Fatalf("message %q collapses the not-attempted case into the outcome-unknown case", text)
	}
}

func TestCallToolSurfacesOutcomeUnknownDistinctlyFromNotAttempted(t *testing.T) {
	fake := testfake.New("slow", tool("slow_tool"))
	hold := make(chan struct{})
	fake.OnCall = func(context.Context, *mcp.CallToolRequest) {
		<-hold // side effect already committed by the time this runs
	}

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return fake.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	// hold must close before srv.Close(), or Close blocks forever waiting for
	// the still-blocked handler; a single ordered cleanup guarantees that
	// regardless of registration order elsewhere in the test.
	t.Cleanup(func() {
		close(hold)
		srv.CloseClientConnections()
		srv.Close()
		fake.Close()
	})

	// TimeoutSec bounds the backend's own call, not the client's: the client
	// waits for a normal (error) response instead of racing the fake to give up
	// locally, which is what actually produces the "outcome unknown" content.
	cfg := &config.Config{Backends: map[string]config.Backend{
		"slow": {Name: "slow", HTTPURL: srv.URL, TimeoutSec: 1},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	reg := backend.NewRegistry(cfg, ov, backend.Hooks{})

	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("slow") })

	client := connectClient(t, NewSearch(cat, reg, rank.Thresholds{}))

	res, err := client.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "call_tool",
		Arguments: map[string]any{"id": "mcp__slow__slow_tool", "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result against a backend that never answers")
	}
	text := textOf(t, res)
	if !strings.Contains(text, "outcome unknown") {
		t.Fatalf("message %q does not say the outcome is unknown", text)
	}
	if !strings.Contains(text, "slow_tool") {
		t.Fatalf("message %q does not name the tool", text)
	}
	if strings.Contains(text, "no send attempted") {
		t.Fatalf("message %q collapses the outcome-unknown case into the not-attempted case", text)
	}
	if got := fake.SideEffects.Load(); got != 1 {
		t.Fatalf("side effect count = %d, want 1 (the call really was delivered)", got)
	}
}

// writeCatalogFile writes a catalog.json a *catalog.Catalog can Load, so a
// test can give it an entry for a backend that must never successfully
// answer a tools/list itself.
func writeCatalogFile(t *testing.T, path string, entries ...catalog.Entry) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Tools []catalog.Entry `json:"tools"`
	}{Tools: entries})
	if err != nil {
		t.Fatalf("marshal catalog fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write catalog fixture: %v", err)
	}
}
