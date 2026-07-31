package web

import (
	"encoding/json"
	"io"
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
	"github.com/ahodges/mcpd/internal/manage"
	"github.com/ahodges/mcpd/internal/mcpsrv"
	"github.com/ahodges/mcpd/internal/oauthstore"
	"github.com/ahodges/mcpd/internal/rank"
	"github.com/ahodges/mcpd/internal/testfake"
)

// harness stands a real Registry, Catalog and web Server over in-process fake
// backends served through streamable HTTP, mirroring internal/mcpsrv's helper. The
// user's configuration is written to a real file, so a test can assert the daemon
// never rewrites it.
type harness struct {
	reg     *backend.Registry
	cat     *catalog.Catalog
	guard   *Guard
	oauth   *oauthstore.Store
	server  *Server
	web     *httptest.Server
	cfgPath string
	statDir string
	mgr     *manage.Manager
	writer  *config.Writer
	ov      *backend.Overrides
}

func newHarness(t *testing.T, fakes ...*testfake.Fake) *harness {
	t.Helper()
	dir := t.TempDir()
	statDir := filepath.Join(dir, "state")

	declared := make(map[string]config.Backend, len(fakes))
	for _, f := range fakes {
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return f.Server() },
			&mcp.StreamableHTTPOptions{Stateless: true},
		))
		t.Cleanup(func() {
			srv.CloseClientConnections()
			srv.Close()
			f.Close()
		})
		declared[f.Name] = config.Backend{HTTPURL: srv.URL, TimeoutSec: 10}
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(config.Config{Backends: declared})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(statDir, "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}

	h := &harness{cfgPath: cfgPath, statDir: statDir}
	h.reg = backend.NewRegistry(cfg, ov, backend.Hooks{
		ToolListChanged: func(s string) { h.cat.Trigger(s) },
		Reconnected:     func(s string) { h.cat.Trigger(s) },
		StopRefresh:     func(s string) { h.cat.StopRefresh(s) },
		DropTools:       func(s string) { h.cat.Drop(s) },
		Refresh:         func(s string) { h.cat.Trigger(s) },
	})
	h.cat = catalog.New(h.reg, filepath.Join(statDir, "catalog.json"))
	h.guard = NewGuard()
	// Unstarted, so the OAuth store can be given the callback URL this very server
	// will answer on rather than a guessed port.
	h.web = httptest.NewUnstartedServer(nil)
	h.oauth = oauthstore.New(statDir, "http://"+h.web.Listener.Addr().String()+"/oauth/callback",
		oauthstore.Hooks{
			NeedsAuth: func(server, note string) {
				if b, ok := h.reg.Get(server); ok {
					b.NoteNeedsAuth(note)
				}
			},
			Authorized: func(server string) {
				if b, ok := h.reg.Get(server); ok {
					b.NoteAuthorized()
				}
			},
		})
	// The management routes are wired the way cmd/mcpd wires them, including the three
	// hooks without which the declared-set protection is inert.
	h.ov = ov
	h.writer, err = config.NewWriter(cfgPath)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	h.mgr = manage.New(h.writer, h.reg, h.cat, ov, h.oauth)
	ov.Guard = func(name string, fn func()) bool { return h.writer.HoldDeclared(name, nil, fn) }
	h.oauth.Declared = h.writer.Identity
	h.oauth.Held = func(server string, want config.Identity, fn func()) bool {
		return h.writer.HoldDeclared(server, &want, fn)
	}
	h.server = New(h.reg, h.cat, h.guard, h.oauth).WithManager(h.mgr)
	h.web.Config.Handler = h.server.Handler()
	h.web.Start()
	t.Cleanup(h.web.Close)
	return h
}

// mcpSurface serves the real search facade behind the same shared guard, so a test
// exercises the MCP endpoint as cmd/mcpd will wire it.
func (h *harness) mcpSurface(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcpsrv.NewSearch(h.cat, h.reg, rank.Thresholds{})
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	ts := httptest.NewServer(h.guard.Protect(handler))
	t.Cleanup(ts.Close)
	return ts
}

// index populates the catalog, so pages and the inspector have tools to render.
func (h *harness) index(t *testing.T) {
	t.Helper()
	h.cat.RefreshAll(t.Context())
}

type reqOpt func(*http.Request)

func origin(o string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Origin", o) }
}

func crossSite() reqOpt {
	return func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }
}

// rebound is what a browser sends once the attacker's name resolves to loopback: it
// believes it is on its own origin, so every origin signal says same-origin and the
// Host header is the only field that still names the attacker.
func rebound(host string) reqOpt {
	return func(r *http.Request) {
		r.Host = host
		r.Header.Set("Origin", "http://"+host)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
}

// contentType sets only the header, so a probe can isolate the method rule from the
// JSON rule: without it a rejected GET proves nothing about which one refused it.
func contentType(kind string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Content-Type", kind) }
}

func jsonBody(body string) reqOpt {
	return func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
	}
}

func mcpInitialize() reqOpt {
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"web-test","version":"test"}}}`
	return func(r *http.Request) {
		r.Header.Set("Accept", "application/json, text/event-stream")
		jsonBody(body)(r)
	}
}

type response struct {
	status int
	body   string
	header http.Header
}

func (r response) denied() bool { return r.status == http.StatusForbidden }

func do(t *testing.T, ts *httptest.Server, method, path string, opts ...reqOpt) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, opt := range opts {
		opt(req)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: res.StatusCode, body: string(body), header: res.Header}
}

func (h *harness) get(t *testing.T, path string, opts ...reqOpt) response {
	t.Helper()
	return do(t, h.web, http.MethodGet, path, opts...)
}

func (h *harness) post(t *testing.T, path string, opts ...reqOpt) response {
	t.Helper()
	return do(t, h.web, http.MethodPost, path, append([]reqOpt{jsonBody(`{}`)}, opts...)...)
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: name + " description",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}}}`),
	}
}
