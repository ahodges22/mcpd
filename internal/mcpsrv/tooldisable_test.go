package mcpsrv

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/rank"
	"github.com/ahodges22/mcpd/internal/testfake"
)

// Scenario (tool-catalog spec, "A disabled tool is unreachable while its backend serves"):
// disabling one tool removes it from both endpoints and never reaches the backend, while
// the backend's other tools keep serving, and an enable restores it without a re-list.
func TestADisabledToolIsUnreachableWhileItsBackendServes(t *testing.T) {
	github := testfake.New("github", tool("create_pull_request"), tool("merge_pull_request"))
	reg := httpRegistry(t, github)
	cat := catalog.New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	cat.RefreshAll(t.Context())
	t.Cleanup(func() { cat.StopRefresh("github") })
	p := NewPassthrough(cat, reg)
	cat.OnCommit(p.Sync)
	ops := NewOperations(cat, reg, rank.Thresholds{}, nil)
	client := connectClient(t, p.Server())
	merge := catalog.CanonicalID("github", "merge_pull_request")

	if err := cat.SetToolEnabled(merge, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	got, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if names, want := toolNames(got.Tools), []string{"mcp__github__create_pull_request"}; !strEqual(names, want) {
		t.Errorf("passthrough tools = %v, want %v", names, want)
	}
	if res, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: merge, Arguments: map[string]any{}}); err == nil && !res.IsError {
		t.Error("passthrough call to a disabled tool succeeded")
	}
	if _, err := ops.Call(t.Context(), CallRequest{ID: merge}); err == nil {
		t.Error("call_tool reached a disabled tool")
	}
	if _, err := ops.Describe(merge); err == nil {
		t.Error("describe_tool described a disabled tool")
	}
	for _, r := range ops.Search(t.Context(), SearchRequest{Query: "merge pull request"}).Results {
		if r.ID == merge {
			t.Error("search_tools offered a disabled tool")
		}
	}
	if slices.Contains(github.Received(), "tools/call:merge_pull_request") {
		t.Error("the backend received a call for a disabled tool")
	}
	if _, err := ops.Call(t.Context(), CallRequest{ID: catalog.CanonicalID("github", "create_pull_request")}); err != nil {
		t.Errorf("the backend's other tool stopped serving: %v", err)
	}

	lists := github.ListCalls.Load()
	if err := cat.SetToolEnabled(merge, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err = client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools after enable: %v", err)
	}
	if !slices.Contains(toolNames(got.Tools), merge) {
		t.Errorf("passthrough tools after enable = %v, want %s restored", toolNames(got.Tools), merge)
	}
	if n := github.ListCalls.Load(); n != lists {
		t.Errorf("enable re-listed the backend %d times, want none", n-lists)
	}
}
