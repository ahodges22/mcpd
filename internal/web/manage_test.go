package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
)

func (h *harness) declared(t *testing.T) map[string]config.Backend {
	t.Helper()
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg.Backends
}

// Scenario (backend-management spec, "An added backend serves tools without a restart"),
// driven as a real request through the real guard and the real mux, because calling the
// handler directly would skip the thing under test.
func TestAddBackendRouteDeclaresAndPublishes(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))

	res := h.post(t, "/api/backends", jsonBody(`{"name":"flint","spec":{"command":"npx"}}`))

	if res.status != http.StatusOK {
		t.Fatalf("POST /api/backends = %d (%s), want 200", res.status, res.body)
	}
	if _, ok := h.declared(t)["flint"]; !ok {
		t.Error("the declaration was not written")
	}
	if _, ok := h.reg.Get("flint"); !ok {
		t.Error("the backend was not published")
	}
}

// Scenario: "A removed backend stops serving".
func TestRemoveBackendRouteUndeclaresAndTearsDown(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))

	res := h.post(t, "/api/backends/alpha/remove")

	if res.status != http.StatusOK {
		t.Fatalf("POST remove = %d (%s), want 200", res.status, res.body)
	}
	if _, ok := h.declared(t)["alpha"]; ok {
		t.Error("the declaration was not removed")
	}
	if _, ok := h.reg.Get("alpha"); ok {
		t.Error("the backend is still routable")
	}
}

// Scenario: "A name containing a path separator or a parent-directory reference is refused
// on the add route and on the remove route, and no file is created or deleted anywhere."
// The remove route matters most: it is the one that deletes a name-derived file.
func TestAManagementRouteRefusesATraversingName(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	// A file under the state directory that a traversing name could reach.
	victim := filepath.Join(h.statDir, "oauth-victim.json")
	if err := os.MkdirAll(h.statDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(victim, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, name := range []string{"../../etc/passwd", "..", "a/b", "Alpha", "x/../../oauth-victim"} {
		body, err := json.Marshal(addRequest{Name: name, Spec: config.Backend{Command: "x"}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if res := h.post(t, "/api/backends", jsonBody(string(body))); res.status != http.StatusBadRequest {
			t.Errorf("add %q = %d (%s), want 400", name, res.status, res.body)
		}
		// Escaped so the traversal reaches the handler as a path value rather than being
		// collapsed by the mux. Any non-2xx counts: the mux normalising a dot segment into
		// a redirect is itself a refusal, because the handler is never reached.
		escaped := strings.ReplaceAll(name, "/", "%2F")
		res := h.post(t, "/api/backends/"+escaped+"/remove")
		if res.status >= 200 && res.status < 300 {
			t.Errorf("remove %q = %d (%s), want a refusal", name, res.status, res.body)
		}
	}

	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a traversing name reached the filesystem: %v", err)
	}
	if got := len(h.declared(t)); got != 1 {
		t.Errorf("declared backends = %d, want 1: a refused name still changed the file", got)
	}
}

// Scenario (loopback-security spec, "The process-spawning route is subject to every
// guard"). Declaring a stdio backend starts a process, so this is the last route that may
// ever be reachable from a browser page.
func TestTheAddRouteIsSubjectToEveryGuard(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	spawn := jsonBody(`{"name":"evil","spec":{"command":"touch","args":["/tmp/mcpd-should-not-exist"]}}`)

	for _, tc := range []struct {
		what string
		res  response
	}{
		{"a cross-site origin", h.post(t, "/api/backends", spawn, origin("https://evil.example"))},
		{"Sec-Fetch-Site: cross-site", h.post(t, "/api/backends", spawn, crossSite())},
		{"a rebound host", h.post(t, "/api/backends", spawn, rebound("attacker.example"))},
	} {
		if !tc.res.denied() {
			t.Errorf("add with %s = %d, want 403", tc.what, tc.res.status)
		}
	}
	// And GET, which is the form a page can trigger by navigation alone.
	if res := h.get(t, "/api/backends"); res.status == http.StatusOK {
		t.Errorf("GET /api/backends = %d, want a refusal", res.status)
	}
	if _, ok := h.declared(t)["evil"]; ok {
		t.Fatal("a guarded-out request still declared a backend that spawns a process")
	}
}

// Scenario: "A write is refused when the file changed underneath it", surfaced to the user
// as a conflict rather than a generic failure, and cleared by a reload rather than a
// restart.
func TestAStaleFileIsAConflictAndAReloadClearsIt(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))

	handEdit := `{"backends":{"alpha":` + declOf(t, h, "alpha") + `,"byhand":{"command":"y"}}}`
	if err := os.WriteFile(h.cfgPath, []byte(handEdit), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res := h.post(t, "/api/backends", jsonBody(`{"name":"flint","spec":{"command":"npx"}}`))
	if res.status != http.StatusConflict {
		t.Fatalf("add against a changed file = %d (%s), want 409", res.status, res.body)
	}
	if !strings.Contains(res.body, "changed on disk") {
		t.Errorf("the response does not say the file changed: %s", res.body)
	}

	if res := h.post(t, "/api/reload"); res.status != http.StatusOK {
		t.Fatalf("POST /api/reload = %d (%s), want 200", res.status, res.body)
	}
	if _, ok := h.reg.Get("byhand"); !ok {
		t.Error("the reload did not adopt the hand-added backend")
	}
	if res := h.post(t, "/api/backends", jsonBody(`{"name":"flint","spec":{"command":"npx"}}`)); res.status != http.StatusOK {
		t.Errorf("add after a reload = %d (%s), want 200: the baseline was not adopted", res.status, res.body)
	}
}

// A duplicate is a conflict and an unknown name is a not-found, so the page can tell the
// user which of the two happened without parsing prose.
func TestManagementRoutesReportTheirRefusalKind(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))

	if res := h.post(t, "/api/backends", jsonBody(`{"name":"alpha","spec":{"command":"x"}}`)); res.status != http.StatusConflict {
		t.Errorf("add of an existing name = %d (%s), want 409", res.status, res.body)
	}
	if res := h.post(t, "/api/backends/nope/remove"); res.status != http.StatusNotFound {
		t.Errorf("remove of an undeclared name = %d (%s), want 404", res.status, res.body)
	}
	if res := h.post(t, "/api/backends", jsonBody(`{"name":"both","spec":{"command":"x","http_url":"https://y.test"}}`)); res.status != http.StatusBadRequest {
		t.Errorf("add naming both transports = %d (%s), want 400", res.status, res.body)
	}
}

// declOf re-encodes a declared backend so a hand edit can preserve it verbatim.
func declOf(t *testing.T, h *harness, name string) string {
	t.Helper()
	raw, err := json.Marshal(h.declared(t)[name])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// Scenario: "Removal requires a second confirming action", and its guardrail, "the
// confirmation is not relied on as a control". The marker is asserted on the rendered page
// and the mechanism is the same data-confirm the inspector already uses, so there is one
// confirmation path rather than two.
func TestTheRemoveActionRequiresASecondConfirmingAction(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))

	page := h.get(t, "/")
	if page.status != http.StatusOK {
		t.Fatalf("GET / = %d", page.status)
	}
	if !strings.Contains(page.body, `data-post="/api/backends/alpha/remove"`) {
		t.Error("the status page offers no remove action")
	}
	if !strings.Contains(page.body, `data-confirm="remove the declaration for alpha"`) {
		t.Error("the remove action carries no confirmation marker")
	}
	if !strings.Contains(page.body, `data-post="/api/reload"`) {
		t.Error("the status page offers no reload action")
	}
	if !strings.Contains(page.body, `id="add-submit"`) {
		t.Error("the status page offers no add form")
	}
}

// The page offers the authorize action for exactly the backends the authorize route will
// accept. Both sides now read the declaration, so this fails if either is rewritten to
// infer it from something else, which is the only way the page can come to offer a button
// that answers "this backend does not authorize with oauth".
func TestTheAuthorizeActionIsOfferedForExactlyTheBackendsTheRouteAccepts(t *testing.T) {
	h := newHarness(t, testfake.New("plain", tool("kubectl_logs")))
	if _, err := h.mgr.Add("secured", config.Backend{
		HTTPURL: "https://mcp.example.test/mcp",
		Auth:    "oauth",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The wait, not the work: the endpoint does not resolve, so the reconnect fails
	// immediately and this only stops the route sitting on a budget it cannot use.
	AuthorizeWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { AuthorizeWaitTimeout = 30 * time.Second })

	page := h.get(t, "/")
	if page.status != http.StatusOK {
		t.Fatalf("GET / = %d", page.status)
	}
	const refusal = "does not authorize with oauth"
	for name, offered := range map[string]bool{"secured": true, "plain": false} {
		action := `data-post="/api/backends/` + name + `/authorize"`
		if got := strings.Contains(page.body, action); got != offered {
			t.Errorf("page offers authorize for %s = %v, want %v", name, got, offered)
		}
		res := h.post(t, "/api/backends/"+name+"/authorize")
		if accepted := !strings.Contains(res.body, refusal); accepted != offered {
			t.Errorf("route accepts authorize for %s = %v, want %v (%d %s)",
				name, accepted, offered, res.status, res.body)
		}
	}
}

// The provider's authorization URL never reaches the page. It is several hundred
// characters of provider-controlled text whose only use is to be followed, and the button
// follows it, so rendering it only gave the user something to copy by hand.
func TestTheStatusPageDoesNotRenderTheProviderAuthorizationURL(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	if b, ok := h.reg.Get("alpha"); ok {
		b.NoteNeedsAuth("authorize at https://evil.example/authorize?client_id=secret-looking-value")
	}

	page := h.get(t, "/")
	if strings.Contains(page.body, "evil.example") {
		t.Errorf("the page renders the provider's authorization URL: %s", page.body)
	}
	// The condition it reports is still there, and so is the action that clears it.
	if !strings.Contains(page.body, "waiting for you to authorize it") {
		t.Errorf("the page does not say the backend needs authorizing: %s", page.body)
	}
}

// The add form must not prefill or display any existing env or headers value, because a
// declaration can carry an inline credential and the page is where one would leak.
func TestTheStatusPageNeverRendersADeclaredCredential(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	if _, err := h.mgr.Add("secretive", config.Backend{
		HTTPURL: "https://example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer super-secret-value"},
		Env:     map[string]string{"TOKEN": "another-secret"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, path := range []string{"/", "/api/status"} {
		body := h.get(t, path).body
		for _, secret := range []string{"super-secret-value", "another-secret", "Authorization"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s exposes %q from a declaration", path, secret)
			}
		}
	}
}
