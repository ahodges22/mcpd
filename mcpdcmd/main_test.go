package mcpdcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/searchindex"
	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/testfake"
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
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
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

func TestWireDegradesSecretControlAuthenticatorFailure(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-control-key"), filepath.Join(state, secretstore.ControlKeyFile)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	overrides, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}

	d := &daemon{state: state, writer: writer, overrides: overrides}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() {
		if d.secretCancel != nil {
			d.secretCancel()
		}
		d.reg.Shutdown()
	})
	if d.secretAuth != nil {
		t.Fatal("secret control authenticator initialized from an unsafe artifact")
	}
}

func TestWireExposesAuthenticatedSecretAPI(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	provider, err := secretstore.NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	overrides, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	d := &daemon{state: state, writer: writer, overrides: overrides}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() {
		if d.secretCancel != nil {
			d.secretCancel()
		}
		d.reg.Shutdown()
	})
	server := httptest.NewServer(d.handler)
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	err = runSecret([]string{
		"set", "--addr", server.URL, "--config", cfgPath, "--state", state, "TOKEN",
	}, secretCommandDeps{
		stdin:      strings.NewReader("daemon-managed-secret\n"),
		stdout:     &stdout,
		httpClient: server.Client(),
		isTerminal: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("runSecret: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "set TOKEN") || strings.Contains(got, "daemon-managed-secret") {
		t.Fatalf("stdout = %q", got)
	}
	stored, err := provider.Get(t.Context(), "TOKEN")
	if err != nil || !stored.Present || stored.Value != "daemon-managed-secret" {
		t.Fatalf("stored result = %#v, error = %v", stored, err)
	}
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

func TestWireResolvesFileSecretIntoHTTPHeader(t *testing.T) {
	const secretName = "MCPD_TEST_WIRE_TOKEN"
	old, present := os.LookupEnv(secretName)
	if err := os.Unsetenv(secretName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(secretName, old)
		} else {
			_ = os.Unsetenv(secretName)
		}
	})

	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	store, err := secretstore.NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Set(t.Context(), secretName, "resolved-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	fake := testfake.New("alpha", tool("kubectl_logs"))
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return fake.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	var authorization atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
		fake.Close()
	})

	cfgPath := filepath.Join(dir, "config.json")
	declaration := config.Config{
		Backends: map[string]config.Backend{
			"alpha": {HTTPURL: upstream.URL, Headers: map[string]string{"Authorization": "Bearer ${" + secretName + "}"}},
		},
		Secrets: &config.Secrets{Provider: config.SecretProviderFile},
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	overrides, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	d := &daemon{state: state, writer: writer, overrides: overrides}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() {
		if d.secretCancel != nil {
			d.secretCancel()
		}
		d.reg.Shutdown()
	})

	d.cat.RefreshAll(t.Context())
	if got, _ := authorization.Load().(string); got != "Bearer resolved-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := d.reg.Health()["alpha"].State; got != backend.StateUp {
		t.Fatalf("alpha state = %q, want up", got)
	}
}

func TestProviderFailureIsolatesDependents(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "secrets.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile secrets: %v", err)
	}

	fake := testfake.New("shared", tool("kubectl_logs"))
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return fake.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
		fake.Close()
	})

	cfgPath := filepath.Join(dir, "config.json")
	declaration := config.Config{
		Backends: map[string]config.Backend{
			"dependent": {HTTPURL: upstream.URL, Headers: map[string]string{"Authorization": "Bearer ${MCPD_TEST_MISSING_TOKEN}"}},
			"unrelated": {HTTPURL: upstream.URL},
		},
		Secrets: &config.Secrets{Provider: config.SecretProviderFile},
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	overrides, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	d := &daemon{state: state, writer: writer, overrides: overrides}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() {
		d.secretCancel()
		d.reg.Shutdown()
	})

	d.cat.RefreshAll(t.Context())
	health := d.reg.Health()
	if health["dependent"].State != backend.StatePending || !strings.Contains(health["dependent"].LastErr, "corrupt") {
		t.Fatalf("dependent health = %#v", health["dependent"])
	}
	if health["unrelated"].State != backend.StateUp {
		t.Fatalf("unrelated health = %#v", health["unrelated"])
	}
}

func TestEmbeddingSecretRebuildsIndexClient(t *testing.T) {
	const secretName = "MCPD_TEST_EMBEDDING_TOKEN"
	old, present := os.LookupEnv(secretName)
	if err := os.Unsetenv(secretName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(secretName, old)
		} else {
			_ = os.Unsetenv(secretName)
		}
	})

	var authorization atomic.Value
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer gateway.Close()

	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if _, err := secretstore.NewFileStore(state); err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	declaration := config.Config{
		Backends: map[string]config.Backend{},
		Embeddings: config.Embeddings{
			URL: gateway.URL, Model: "embed", APIKeyEnv: secretName,
		},
		Secrets: &config.Secrets{Provider: config.SecretProviderFile},
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	overrides, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	d := &daemon{state: state, writer: writer, overrides: overrides}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() {
		d.secretCancel()
		d.reg.Shutdown()
	})

	if _, err := d.secrets.Set(t.Context(), secretName, "new-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries := []catalog.Entry{{ID: "mcp__alpha__tool", Server: "alpha", Tool: "tool", Description: "tool"}}
	if _, _, err := d.index.Search(t.Context(), "tool", entries, 1); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, _ := authorization.Load().(string); got != "Bearer new-key" {
		t.Fatalf("Authorization = %q", got)
	}
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
	writer, cfg, err := config.NewWriter(d.writer.Path())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(d.state, "overrides.json"), writer)
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

// The daemon has no user authentication, so it must refuse to bind anywhere but loopback:
// a non-loopback listener would expose every connected tool to the network and drop the
// MCP endpoints' rebinding defense.
func TestRequireLoopbackAddrRefusesNonLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7420", true},
		{"localhost:7420", true},
		{"[::1]:7420", true},
		{"127.0.0.2:0", true},
		{":7420", false},
		{"0.0.0.0:7420", false},
		{"192.168.1.10:7420", false},
		{"example.com:7420", false},
		{"127.0.0.1", false},
	} {
		err := requireLoopbackAddr(tc.addr)
		if tc.ok && err != nil {
			t.Errorf("requireLoopbackAddr(%q) = %v, want nil", tc.addr, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("requireLoopbackAddr(%q) = nil, want an error", tc.addr)
		}
	}
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

	// And with a gateway sending the calibrated model, the number reaches the facade.
	d.index = searchindex.New(t.TempDir(), config.Embeddings{URL: "https://gateway.test", Model: abstainModel}, config.Ranking{})
	got := d.thresholds()
	if !got.Enabled || got.Cosine != abstainCosine {
		t.Errorf("thresholds = %+v, want the calibrated %v enabled", got, abstainCosine)
	}

	// Any other model gets no abstention: the threshold was not measured for its vectors.
	d.index = searchindex.New(t.TempDir(), config.Embeddings{URL: "https://gateway.test"}, config.Ranking{})
	if got := d.thresholds(); got.Enabled {
		t.Errorf("abstention is enabled for an uncalibrated embedding model: %+v", got)
	}
}

func TestWireStartsRemoteFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"remote":{"enabled":true,"addr":"127.0.0.1:0","advertise":"https://mcpd.home.example"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, cfg, err := config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "remote-token"),
		[]byte("00112233445566778899aabbccddeeff\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(state, "overrides.json"), writer)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}

	d := &daemon{state: state, writer: writer, overrides: ov}
	if err := d.wire(cfg, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(d.reg.Shutdown)
	t.Cleanup(d.remote.Close)
	// wire builds the lifecycle but must not bind it: serve starts it only
	// after the main listener holds its own port, so an overlapping remote
	// address can never take the daemon down.
	if d.remote.Running() {
		t.Fatal("wire bound the remote listener before the main listener")
	}
	d.remote.Apply()
	if !d.remote.Running() {
		t.Fatal("remote listener not restored from config")
	}
	if got := d.remote.Advertise(); got != "https://mcpd.home.example" {
		t.Fatalf("advertised origin not restored across restart: %q", got)
	}

	// A config with no remote declaration starts nothing.
	dir2 := t.TempDir()
	cfgPath2 := filepath.Join(dir2, "config.json")
	if err := os.WriteFile(cfgPath2, []byte(`{"backends":{}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer2, cfg2, err := config.NewWriter(cfgPath2)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	state2 := filepath.Join(dir2, "state")
	if err := os.MkdirAll(state2, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ov2, err := backend.LoadOverrides(filepath.Join(state2, "overrides.json"), writer2)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	d2 := &daemon{state: state2, writer: writer2, overrides: ov2}
	if err := d2.wire(cfg2, "127.0.0.1:0"); err != nil {
		t.Fatalf("wire without remote: %v", err)
	}
	t.Cleanup(d2.reg.Shutdown)
	t.Cleanup(d2.remote.Close)
	if d2.remote.Running() || d2.remote.Declared() {
		t.Fatal("remote listener started without a declaration")
	}
}
