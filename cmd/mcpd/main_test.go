package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/searchindex"
	"github.com/ahodges/mcpd/internal/testfake"
)

// wireDaemon stands the daemon up exactly as run does, minus the listener, over one fake
// upstream. This is the first test in the project that exercises the whole wiring: every
// other package is tested on its own, so a hook nobody connected has been invisible until
// here.
func wireDaemon(t *testing.T, tools ...*mcp.Tool) (*daemon, *httptest.Server) {
	t.Helper()
	fake := testfake.New("alpha", tools...)
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return fake.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
		fake.Close()
	})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"backends":{"alpha":{"http_url":"` + upstream.URL + `","timeout":10}}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	writer, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"))
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}

	d := &daemon{state: state, writer: writer, overrides: ov}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(d.reg.Shutdown)

	ts := httptest.NewServer(d.handler)
	t.Cleanup(ts.Close)
	return d, ts
}

func rpc(t *testing.T, ts *httptest.Server, path, method string) string {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{}}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s = %d", path, res.StatusCode)
	}
	return path
}

// Scenario (mcp-endpoints spec): all three surfaces answer, and the two MCP endpoints are
// mounted on distinct paths behind the same guard value.
func TestAllThreeSurfacesAnswer(t *testing.T) {
	d, ts := wireDaemon(t, tool("kubectl_logs"), tool("kubectl_get"))
	d.cat.RefreshAll(t.Context())

	status, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer status.Body.Close()
	if status.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status = %d", status.StatusCode)
	}
	var snapshot struct {
		ToolCount int `json:"tool_count"`
		Backends  []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"backends"`
	}
	if err := json.NewDecoder(status.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(snapshot.Backends) != 1 || snapshot.Backends[0].Name != "alpha" {
		t.Fatalf("backends = %+v, want one named alpha", snapshot.Backends)
	}
	if snapshot.Backends[0].State != string(backend.StateUp) {
		t.Errorf("alpha state = %q, want up", snapshot.Backends[0].State)
	}
	if snapshot.ToolCount != 2 {
		t.Errorf("tool count = %d, want 2", snapshot.ToolCount)
	}

	// Both MCP endpoints are mounted and reachable. The facade advertises its three
	// tools regardless of the catalog; pass-through advertises the catalog itself, so the
	// two counts differ, which is the point of having both.
	for _, path := range []string{"/mcp/search", "/mcp/passthrough"} {
		rpc(t, ts, path, "initialize")
	}
}

// A cross-site origin is refused on both MCP endpoints and on the web surface, because all
// three are wrapped in the same guard value rather than three configurations that drift.
func TestOneGuardCoversEverySurface(t *testing.T) {
	_, ts := wireDaemon(t, tool("kubectl_logs"))

	for _, path := range []string{"/mcp/search", "/mcp/passthrough", "/api/reindex", "/api/backends"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("cross-origin POST %s = %d, want 403", path, res.StatusCode)
		}
	}
}

// Scenario (backend-management spec) end to end through the daemon's own wiring: a backend
// declared over HTTP is published, serves, and is gone again after a removal.
func TestABackendCanBeAddedAndRemovedThroughTheDaemon(t *testing.T) {
	d, ts := wireDaemon(t, tool("kubectl_logs"))

	added := testfake.New("beta", tool("open_pull_request"))
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return added.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
		added.Close()
	})

	body := `{"name":"beta","spec":{"http_url":"` + upstream.URL + `","timeout":10}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/backends", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/backends: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/backends = %d", res.StatusCode)
	}

	d.cat.RefreshAll(t.Context())
	if _, ok := d.reg.Get("beta"); !ok {
		t.Fatal("the added backend was not published")
	}
	found := false
	for _, e := range d.cat.Entries() {
		if e.Server == "beta" && e.Tool == "open_pull_request" {
			found = true
		}
	}
	if !found {
		t.Error("the added backend's tools never reached the catalog")
	}

	removeReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/backends/beta/remove", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	removeReq.Header.Set("Content-Type", "application/json")
	removed, err := ts.Client().Do(removeReq)
	if err != nil {
		t.Fatalf("POST remove: %v", err)
	}
	removed.Body.Close()
	if removed.StatusCode != http.StatusOK {
		t.Fatalf("POST remove = %d", removed.StatusCode)
	}
	if _, ok := d.reg.Get("beta"); ok {
		t.Error("the removed backend is still routable")
	}
	for _, e := range d.cat.Entries() {
		if e.Server == "beta" {
			t.Error("the removed backend's tools are still in the catalog")
		}
	}
}

// Scenario (task 11.2a): a crash between an override write and its tool eviction leaves a
// disabled backend's tools in the persisted catalog, and a disabled backend is never
// re-listed, so startup is the only thing that ever removes them.
func TestStartupDropsTheToolsOfABackendThatLoadedDisabled(t *testing.T) {
	d, _ := wireDaemon(t, tool("kubectl_logs"))
	d.cat.RefreshAll(t.Context())
	if len(d.cat.Entries()) == 0 {
		t.Fatal("no tools to persist")
	}
	// The crash, staged by writing the override file directly. Going through Disable would
	// not reproduce it: Disable evicts the tools itself, so the persisted catalog would
	// already be clean and the test would pass whether or not startup evicts anything.
	id := config.IdentityOf(cfgOf(t, d)["alpha"])
	entry, err := json.Marshal(map[string]any{"disabled": map[string]config.Identity{"alpha": id}})
	if err != nil {
		t.Fatalf("marshal overrides: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d.state, "overrides.json"), entry, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := d.cat.Save(); err != nil {
		t.Fatalf("Save catalog: %v", err)
	}

	// The restart, over the same state, with the tools still on disk.
	cfg := cfgFull(t, d)
	writer, err := config.NewWriter(d.writer.Path())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(d.state, "overrides.json"))
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	restarted := &daemon{state: d.state, writer: writer, overrides: ov}
	if err := restarted.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(restarted.reg.Shutdown)

	if got := restarted.reg.Health()["alpha"].State; got != backend.StateDisabled {
		t.Fatalf("alpha state after restart = %q, want disabled", got)
	}
	for _, e := range restarted.cat.Entries() {
		if e.Server == "alpha" {
			t.Fatal("a disabled backend's tools survived startup, and nothing else ever removes them")
		}
	}
}

func cfgOf(t *testing.T, d *daemon) map[string]config.Backend {
	t.Helper()
	return cfgFull(t, d).Backends
}

func cfgFull(t *testing.T, d *daemon) *config.Config {
	t.Helper()
	cfg, err := config.Load(d.writer.Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: name + " description"}
}

// Abstention is inert unless it is wired, and an unwired threshold is invisible: search keeps
// answering, just without ever saying it found nothing good. Task 7 built the mechanism and
// Task 13 calibrated the number, so this asserts the one thing neither of those can: that the
// daemon actually hands the calibrated value to the facade, and that it stays off when there
// is no gateway to produce a cosine to judge against.
func TestAbstentionIsWiredOnlyWhenAGatewayCanProduceACosine(t *testing.T) {
	if abstainCosine <= 0 {
		t.Skip("no threshold is calibrated, so there is nothing to wire")
	}
	d, _ := wireDaemon(t, tool("kubectl_logs"))
	if d.index != nil {
		t.Fatal("this harness configures no embeddings gateway, so the case below is not the one being tested")
	}
	if got := d.thresholds(); got.Enabled {
		t.Errorf("abstention is enabled with no gateway: %+v", got)
	}

	// And with a gateway, the calibrated number reaches the facade.
	d.index = searchindex.New(t.TempDir(), "https://gateway.test", "", "", "", "", 0)
	got := d.thresholds()
	if !got.Enabled || got.Cosine != abstainCosine {
		t.Errorf("thresholds = %+v, want the calibrated %v enabled", got, abstainCosine)
	}
}
