package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/testfake"
)

func TestACrossSiteOriginIsRejectedOnAnMCPEndpoint(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	mcp := h.mcpSurface(t)

	byOrigin := do(t, mcp, http.MethodPost, "/", mcpInitialize(), origin("https://evil.example"))
	if !byOrigin.denied() {
		t.Errorf("a cross-origin initialize got %d, want %d: a page in an open tab can drive the MCP endpoint",
			byOrigin.status, http.StatusForbidden)
	}
	bySecFetch := do(t, mcp, http.MethodPost, "/", mcpInitialize(), crossSite())
	if !bySecFetch.denied() {
		t.Errorf("a Sec-Fetch-Site: cross-site initialize got %d, want %d", bySecFetch.status, http.StatusForbidden)
	}
}

func TestAForeignOriginIsRejectedOnTheStatusAPI(t *testing.T) {
	// The web routes are exercised with POST, not GET: GET, HEAD and OPTIONS are safe
	// methods to CrossOriginProtection and are always allowed, so a GET would pass
	// whether the guard were wrapped around the mux or not and would prove nothing.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	h := newHarness(t, fake)

	res := h.post(t, "/api/reindex", origin("https://evil.example"))
	if !res.denied() {
		t.Fatalf("a cross-origin re-index got %d, want %d: the web routes are not behind the guard",
			res.status, http.StatusForbidden)
	}
	if n := fake.ListCalls.Load(); n != 0 {
		t.Errorf("the rejected re-index still re-listed the backend %d times", n)
	}
}

func TestARequestWithNoOriginIsAcceptedOnBothSurfaces(t *testing.T) {
	// Every native MCP client sends neither header, so rejecting them would break
	// every non-browser caller.
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	mcp := h.mcpSurface(t)

	if res := do(t, mcp, http.MethodPost, "/", mcpInitialize()); res.status != http.StatusOK {
		t.Errorf("initialize with no origin got %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if res := h.post(t, "/api/reindex"); res.status != http.StatusOK {
		t.Errorf("re-index with no origin got %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
}

func TestTheProtectionIsActiveRatherThanAssumed(t *testing.T) {
	// The rejection is asserted through the deny handler this daemon configured, so
	// an unguarded surface cannot pass by resembling a library default.
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	mcp := h.mcpSurface(t)

	web := h.post(t, "/api/reindex", crossSite())
	if !web.denied() || !strings.Contains(web.body, denyReason) {
		t.Errorf("web rejection = %d %q, want %d carrying the configured reason", web.status, web.body, http.StatusForbidden)
	}
	endpoint := do(t, mcp, http.MethodPost, "/", mcpInitialize(), crossSite())
	if !endpoint.denied() || !strings.Contains(endpoint.body, denyReason) {
		t.Errorf("MCP rejection = %d %q, want %d carrying the configured reason", endpoint.status, endpoint.body, http.StatusForbidden)
	}
}

func TestThePolicyCannotDivergeBetweenSurfaces(t *testing.T) {
	// The policy is changed after both surfaces exist, which is the only assertion
	// that distinguishes one shared value from two identical constructions.
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	mcp := h.mcpSurface(t)
	const trusted = "https://trusted.example"

	if res := h.post(t, "/api/reindex", origin(trusted)); !res.denied() {
		t.Fatalf("the origin was already trusted before it was added: got %d", res.status)
	}
	if err := h.guard.protection.AddTrustedOrigin(trusted); err != nil {
		t.Fatalf("add trusted origin: %v", err)
	}

	if res := h.post(t, "/api/reindex", origin(trusted)); res.denied() {
		t.Errorf("the web routes still reject %s, so they hold their own protection value", trusted)
	}
	if res := do(t, mcp, http.MethodPost, "/", mcpInitialize(), origin(trusted)); res.denied() {
		t.Errorf("the MCP endpoint still rejects %s, so it holds its own protection value", trusted)
	}
}

func TestEveryMutationRouteIsRejectedOnGET(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	for _, rt := range h.server.routes() {
		if !rt.mutates {
			continue
		}
		path := concretePath(rt.path, "alpha")
		// The GET carries a JSON content type, so only the method rule can reject it.
		if res := h.get(t, path, contentType("application/json")); res.status != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want %d: a navigation or an image load reaches it",
				path, res.status, http.StatusMethodNotAllowed)
		}
		// And a POST carrying what a cross-origin form submission can send is rejected
		// by the JSON rule, which is the other half of the same requirement.
		if res := h.post(t, path, contentType("application/x-www-form-urlencoded")); res.status != http.StatusUnsupportedMediaType {
			t.Errorf("form POST %s = %d, want %d: a cross-origin form submission reaches it",
				path, res.status, http.StatusUnsupportedMediaType)
		}
	}
}

func TestNoRouteChangesStateOnGET(t *testing.T) {
	// The set is derived from the routes that are registered rather than from a
	// literal, so a route added without a POST declaration is caught here. Task 10's
	// OAuth callback becomes the single documented member.
	fake := testfake.New("alpha", tool("kubectl_logs"))
	h := newHarness(t, fake)
	h.index(t)
	listsBefore := fake.ListCalls.Load()

	var reachable []string
	for _, rt := range h.server.routes() {
		if !rt.mutates {
			continue
		}
		if rt.method != http.MethodPost {
			reachable = append(reachable, rt.path)
			continue
		}
		// The probe carries a JSON content type, so the method rule is the only thing
		// that can reject it and the JSON rule cannot stand in for it.
		if res := h.get(t, concretePath(rt.path, "alpha"), contentType("application/json")); res.status < 400 {
			reachable = append(reachable, rt.path)
		}
	}
	if len(reachable) != 0 {
		t.Errorf("routes that change state on GET = %v, want none", reachable)
	}

	if got := h.reg.Health()["alpha"].State; got != backend.StateUp {
		t.Errorf("alpha state = %q, want %q: a GET changed backend state", got, backend.StateUp)
	}
	if n := fake.ListCalls.Load(); n != listsBefore {
		t.Errorf("list calls = %d, want %d: a GET re-listed the backend", n, listsBefore)
	}
	if n := fake.SideEffects.Load(); n != 0 {
		t.Errorf("side effects = %d, want 0: a GET invoked a tool", n)
	}
	if _, err := os.Stat(filepath.Join(h.statDir, "overrides.json")); !os.IsNotExist(err) {
		t.Errorf("an override was written by a GET (stat err = %v)", err)
	}
}

func concretePath(pattern, name string) string {
	return strings.ReplaceAll(pattern, "{name}", name)
}
