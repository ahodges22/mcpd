package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/testfake"
)

// The admin API toggles one tool, reports its state in the listing, and refuses an id
// the catalog does not list.
func TestTheAdminAPITogglesOneTool(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs"), tool("kubectl_delete")))
	h.index(t)
	admin := httptest.NewServer(h.server.AdminHandler())
	t.Cleanup(admin.Close)
	post := func(path string) int {
		t.Helper()
		res, err := http.Post(admin.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	listed := func() map[string]bool {
		t.Helper()
		res, err := http.Get(admin.URL + "/tools")
		if err != nil {
			t.Fatalf("GET /tools: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Tools []struct {
				ID       string `json:"id"`
				Disabled bool   `json:"disabled"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode /tools: %v", err)
		}
		out := make(map[string]bool, len(body.Tools))
		for _, tool := range body.Tools {
			out[tool.ID] = tool.Disabled
		}
		return out
	}

	if got := post("/tools/mcp__alpha__kubectl_delete/disable"); got != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", got)
	}
	if got := listed(); len(got) != 2 || !got["mcp__alpha__kubectl_delete"] || got["mcp__alpha__kubectl_logs"] {
		t.Errorf("listing after disable = %v, want both tools with only kubectl_delete disabled", got)
	}
	if got := post("/tools/mcp__alpha__kubectl_delete/enable"); got != http.StatusOK {
		t.Fatalf("enable status = %d, want 200", got)
	}
	if got := listed(); got["mcp__alpha__kubectl_delete"] {
		t.Errorf("listing after enable = %v, want kubectl_delete enabled", got)
	}
	if got := post("/tools/mcp__alpha__missing/disable"); got != http.StatusNotFound {
		t.Errorf("unknown tool status = %d, want 404", got)
	}
}
