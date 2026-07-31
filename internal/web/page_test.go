package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/testfake"
)

const scriptPayload = `<script>alert("pwned")</script>`

// TestAMaliciousToolResultIsCarriedAsEscapedJSON pins the transport half of "a
// malicious tool result is inert" and nothing more: the payload leaves the daemon as
// escaped JSON text under a nosniff header. The DOM half, that the value is inserted
// through textContent and never as markup, is owned by
// TestNoMarkupInsertionAPIInTheAssets, which is the test that fails if setText
// changes. This one passes either way, and its name now says only what it proves.
func TestAMaliciousToolResultIsCarriedAsEscapedJSON(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	fake.Server().AddTool(
		&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: scriptPayload}}}, nil
		})
	h := newHarness(t, fake)
	h.index(t)

	res := h.post(t, "/api/invoke", jsonBody(`{"id":"mcp__alpha__echo","arguments":{}}`))
	if res.status != http.StatusOK {
		t.Fatalf("invoke = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if strings.Contains(res.body, "<script") {
		t.Errorf("the response carries live markup, so inserting it as markup would execute it: %s", res.body)
	}
	// The payload's only form in the body is the JSON encoding of a string, whose
	// angle brackets are escaped, and the page inserts it through textContent.
	encoded, err := json.Marshal(scriptPayload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if want := string(bytes.Trim(encoded, `"`)); !strings.Contains(res.body, want) {
		t.Errorf("the payload is not carried as escaped JSON text (%s): %s", want, res.body)
	}
	if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff: a JSON body of backend text must not be sniffed as HTML", got)
	}
}

func TestAMaliciousToolDescriptionIsInert(t *testing.T) {
	evil := &mcp.Tool{
		Name:        "kubectl_logs",
		Description: scriptPayload,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	h := newHarness(t, testfake.New("alpha", evil))
	h.index(t)

	res := h.get(t, "/inspect/alpha")
	if res.status != http.StatusOK {
		t.Fatalf("inspector = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if strings.Contains(res.body, "<script>alert") {
		t.Errorf("the description reached the page as markup: %s", res.body)
	}
	if !strings.Contains(res.body, "&lt;script&gt;") {
		t.Errorf("the description was not rendered as escaped text: %s", res.body)
	}
	const csp = "default-src 'self'; frame-ancestors 'none'"
	if got := res.header.Get("Content-Security-Policy"); got != csp {
		t.Errorf("Content-Security-Policy = %q, want %q", got, csp)
	}
}

func TestReindexReListsEveryEnabledBackend(t *testing.T) {
	alpha := testfake.New("alpha", tool("kubectl_logs"))
	beta := testfake.New("beta", tool("open_pull_request"))
	h := newHarness(t, alpha, beta)
	h.index(t)
	if res := h.post(t, "/api/backends/beta/disable"); res.status != http.StatusOK {
		t.Fatalf("disable beta = %d (%s)", res.status, res.body)
	}
	alphaBefore, betaBefore := alpha.ListCalls.Load(), beta.ListCalls.Load()

	if res := h.post(t, "/api/reindex"); res.status != http.StatusOK {
		t.Fatalf("re-index = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if n := alpha.ListCalls.Load(); n <= alphaBefore {
		t.Errorf("alpha list calls = %d, want more than %d: the enabled backend was not re-listed", n, alphaBefore)
	}
	if n := beta.ListCalls.Load(); n != betaBefore {
		t.Errorf("beta list calls = %d, want %d: a disabled backend was re-listed", n, betaBefore)
	}
	if _, ok := h.cat.Lookup(catalog.CanonicalID("alpha", "kubectl_logs")); !ok {
		t.Error("the catalog lost alpha's tool across the re-index")
	}
}

func TestDisablingFromTheAPILeavesTheUsersConfigUntouched(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if res := h.post(t, "/api/backends/alpha/disable"); res.status != http.StatusOK {
		t.Fatalf("disable = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}

	after, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the daemon rewrote the user's configuration file: %s", after)
	}
	override, err := os.ReadFile(filepath.Join(h.statDir, "overrides.json"))
	if err != nil {
		t.Fatalf("read overrides: %v", err)
	}
	if !strings.Contains(string(override), "alpha") {
		t.Errorf("overrides = %s, want alpha recorded in the state directory", override)
	}
	if got := h.reg.Health()["alpha"].State; got != backend.StateDisabled {
		t.Errorf("state = %q, want %q", got, backend.StateDisabled)
	}
}

func TestATransitionAfterAShutdownIsUnavailableRatherThanAnError(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	h.reg.Shutdown()

	res := h.post(t, "/api/backends/alpha/reconnect")
	if res.status != http.StatusServiceUnavailable {
		t.Errorf("reconnect after a shutdown = %d (%s), want %d: a daemon on its way out is not broken",
			res.status, res.body, http.StatusServiceUnavailable)
	}
	// The reads keep answering, so the page can still say what happened.
	if got := h.get(t, "/api/status"); got.status != http.StatusOK {
		t.Errorf("status read after a shutdown = %d, want %d", got.status, http.StatusOK)
	}
}

func TestADisabledBackendRendersAsDisabledRatherThanFailing(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	h.index(t)
	if err := h.reg.Disable("alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	res := h.get(t, "/")
	if res.status != http.StatusOK {
		t.Fatalf("status page = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if !strings.Contains(res.body, `data-state="disabled"`) {
		t.Errorf("the page does not render alpha as disabled: %s", res.body)
	}
	// The lamp colour as well as the state, because the two are rendered from separate
	// fields and a backend shown as off in one and as broken in the other is still wrong.
	for _, failing := range []string{`data-tone="fault"`, `data-tone="cold"`} {
		if strings.Contains(res.body, failing) {
			t.Errorf("the page renders a backend the user turned off with %s", failing)
		}
	}
	// And it asks nothing of the user: a backend the user turned off is not an alarm.
	if !strings.Contains(res.body, "Nothing needs you") {
		t.Errorf("a backend the user turned off was raised as something needing attention: %s", res.body)
	}
}

func TestTheInspectorSurfacesToolsNeedingConfirmation(t *testing.T) {
	destructive := true
	tools := []*mcp.Tool{
		{Name: "delete_cluster", InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}},
		{Name: "unannotated", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "kubectl_logs", InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
	}
	h := newHarness(t, testfake.New("alpha", tools...))
	h.index(t)

	sections := toolSections(t, h.get(t, "/inspect/alpha"))
	for id, want := range map[string]string{
		"mcp__alpha__delete_cluster": `data-confirm="destructive"`,
		"mcp__alpha__unannotated":    `data-confirm="carrying no read-only annotation"`,
		"mcp__alpha__kubectl_logs":   `data-confirm=""`,
	} {
		section, ok := sections[id]
		if !ok {
			t.Fatalf("the inspector does not list %s: %v", id, sections)
		}
		if !strings.Contains(section, want) {
			t.Errorf("%s section does not carry %s: %s", id, want, section)
		}
	}
}

// toolSections splits the inspector page by tool, so an assertion about one tool
// cannot be satisfied by another tool's markup.
func toolSections(t *testing.T, res response) map[string]string {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("inspector = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	const open = `<section class="tool" data-id="`
	out := make(map[string]string)
	for _, part := range strings.Split(res.body, open)[1:] {
		id, rest, _ := strings.Cut(part, `"`)
		section, _, _ := strings.Cut(rest, "</section>")
		out[id] = section
	}
	return out
}
