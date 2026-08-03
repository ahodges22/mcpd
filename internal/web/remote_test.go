package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/testfake"
)

// remoteReq is httptest.NewRequest with a private peer, so the peer gate is
// out of the way everywhere except the test that targets it:
// httptest.NewRequest defaults RemoteAddr to 192.0.2.1, a public TEST-NET
// address the gate refuses.
func remoteReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "192.168.1.2:1"
	return req
}

const testToken = "00112233445566778899aabbccddeeff"

func TestRemoteAuthTokenGate(t *testing.T) {
	h := newHarness(t)
	rh := h.server.remoteHandler(func() string { return testToken })

	// No cookie, no token: 403.
	rr := httptest.NewRecorder()
	rh.ServeHTTP(rr, remoteReq("GET", "/api/status", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no auth: got %d", rr.Code)
	}
	// Bad token: 403, no cookie set.
	rr = httptest.NewRecorder()
	rh.ServeHTTP(rr, remoteReq("GET", "/?token=deadbeefdeadbeefdeadbeefdeadbeef", nil))
	if rr.Code != http.StatusForbidden || len(rr.Result().Cookies()) != 0 {
		t.Fatalf("bad token: got %d cookies %v", rr.Code, rr.Result().Cookies())
	}
	// Good token: redirect with cookie; the cookie then authenticates.
	rr = httptest.NewRecorder()
	rh.ServeHTTP(rr, remoteReq("GET", "/?token="+testToken, nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("pairing: got %d", rr.Code)
	}
	cookie := rr.Result().Cookies()[0]
	req := remoteReq("GET", "/api/status", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	rh.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("with cookie: got %d", rr.Code)
	}
	// A public peer is refused even with a valid cookie.
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(cookie)
	req.RemoteAddr = "203.0.113.9:4444"
	rr = httptest.NewRecorder()
	rh.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("public peer: got %d, want 403", rr.Code)
	}
}

// The remote callback route is the loopback route: same handler, same nonce
// guard, no token required. A state matching no outstanding authorization is
// refused identically on both listeners, which pins the shared wiring; the
// delivery success path is owned by the oauthstore flow tests.
func TestRemoteCallbackMatchesLoopbackBehavior(t *testing.T) {
	h := newHarness(t)
	rh := h.server.remoteHandler(func() string { return testToken })

	loop := h.get(t, "/oauth/callback?state=x&code=y")
	rr := httptest.NewRecorder()
	rh.ServeHTTP(rr, remoteReq("GET", "/oauth/callback?state=x&code=y", nil))
	if rr.Code != http.StatusBadRequest || loop.status != http.StatusBadRequest {
		t.Fatalf("remote %d, loopback %d, want 400 on both", rr.Code, loop.status)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != strings.TrimSpace(loop.body) {
		t.Fatalf("refusals differ: remote %q, loopback %q", got, loop.body)
	}
}

func TestRemotePastedCallback(t *testing.T) {
	h := newHarness(t)
	rh := h.server.remoteHandler(func() string { return testToken })
	post := func(body string) *httptest.ResponseRecorder {
		req := remoteReq("POST", "/api/callback", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "mcpd_remote", Value: testToken})
		rr := httptest.NewRecorder()
		rh.ServeHTTP(rr, req)
		return rr
	}

	// A URL that is not a callback URL at all.
	if rr := post(`{"url":"http://127.0.0.1:7420/oauth/callback"}`); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "state and code") {
		t.Fatalf("parameterless URL: %d %s", rr.Code, rr.Body)
	}
	// A well-formed callback whose state matches nothing reaches Deliver and is
	// refused there, which proves the paste path ends in the same delivery.
	if rr := post(`{"url":"http://127.0.0.1:7420/oauth/callback?code=X&state=Y"}`); rr.Code != http.StatusBadRequest ||
		!strings.Contains(rr.Body.String(), "no outstanding authorization") {
		t.Fatalf("unknown state: %d %s", rr.Code, rr.Body)
	}
}

// The security boundary: fully authenticated requests, JSON where relevant,
// must get 404 for every main-listener route that is not part of the remote
// surface. 403 is not a pass, because a 403 can come from middleware in front
// of an accidentally mounted handler.
func TestRemoteForbiddenRoutesAbsent(t *testing.T) {
	h := newHarness(t, testfake.New("x", tool("kubectl_logs")))
	rh := h.server.remoteHandler(func() string { return testToken })
	forbidden := []struct{ method, path string }{
		{"POST", "/mcp/passthrough"}, {"POST", "/mcp/search"},
		{"POST", "/api/invoke"}, {"POST", "/api/backends"},
		{"POST", "/api/backends/x/remove"}, {"POST", "/api/reload"},
		{"POST", "/api/reindex"}, {"GET", "/inspect/x"},
		{"POST", "/api/backends/x/enable"}, {"POST", "/api/backends/x/disable"},
		{"POST", "/api/backends/x/reconnect"}, {"POST", "/api/reconnect-all"},
		{"POST", "/api/remote"},
	}
	for _, f := range forbidden {
		req := remoteReq(f.method, f.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "mcpd_remote", Value: testToken})
		rr := httptest.NewRecorder()
		rh.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404", f.method, f.path, rr.Code)
		}
	}
}

// fakeRemoteWriter records SetRemote calls and can refuse them.
type fakeRemoteWriter struct {
	calls      int
	lastRemote config.Remote
	fail       bool
}

func (f *fakeRemoteWriter) SetRemote(r config.Remote) ([]error, error) {
	if f.fail {
		return nil, config.ErrStale
	}
	f.calls++
	f.lastRemote = r
	return nil, nil
}

func newTestRemote(t *testing.T, addr string) (*Remote, *fakeRemoteWriter) {
	t.Helper()
	h := newHarness(t)
	w := &fakeRemoteWriter{}
	rc := NewRemote(h.server, w, filepath.Join(t.TempDir(), "remote-token"), addr, "desk")
	t.Cleanup(rc.Close)
	return rc, w
}

func TestRemoteEnableDisableLifecycle(t *testing.T) {
	rc, _ := newTestRemote(t, "127.0.0.1:0")
	urls, err := rc.Enable()
	if err != nil || len(urls) == 0 {
		t.Fatalf("enable: %v %v", urls, err)
	}
	tok1, ok := loadRemoteToken(rc.tokenPath)
	if !ok {
		t.Fatal("enable stored no token")
	}
	// The listener answers, and the URL carries the real bound port.
	resp, err := http.Get(urls[0])
	if err != nil {
		t.Fatalf("listener not serving: %v", err)
	}
	resp.Body.Close()
	bound := rc.ln.Addr().(*net.TCPAddr).Port
	if u, _ := url.Parse(urls[0]); u.Port() != strconv.Itoa(bound) {
		t.Fatalf("URL port %s, bound %d", u.Port(), bound)
	}
	// Idempotent enable: same token, same URLs.
	again, err := rc.Enable()
	if err != nil || !slices.Equal(urls, again) {
		t.Fatalf("re-enable rotated: %v vs %v (%v)", urls, again, err)
	}
	if err := rc.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := loadRemoteToken(rc.tokenPath); ok {
		t.Fatal("token survived disable")
	}
	if _, err := http.Get(urls[0]); err == nil {
		t.Fatal("listener survived disable")
	}
	if _, err := rc.Enable(); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	tok2, _ := loadRemoteToken(rc.tokenPath)
	if tok1 == tok2 {
		t.Fatal("token not rotated across disable/enable")
	}
}

// Disable after a failed startup still retracts config: declared and running
// are separate states.
func TestRemoteDisableAfterFailedStartup(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	rc, w := newTestRemote(t, blocker.Addr().String())
	tok, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, tok); err != nil {
		t.Fatal(err)
	}
	rc.Apply(config.Remote{Enabled: true}) // cannot bind; declared stays true
	if rc.Running() {
		t.Fatal("precondition: listener should be off")
	}
	if !rc.Declared() {
		t.Fatal("failed startup must leave the declaration standing")
	}
	if err := rc.Disable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if w.lastRemote.Enabled || w.calls == 0 {
		t.Fatal("disable did not persist enabled:false")
	}
	if _, ok := loadRemoteToken(rc.tokenPath); ok {
		t.Fatal("disable left the token behind")
	}
}

// A failed enable must not destroy a pre-existing valid token.
func TestRemoteEnableRollbackKeepsPriorToken(t *testing.T) {
	rc, w := newTestRemote(t, "127.0.0.1:0")
	prior, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, prior); err != nil {
		t.Fatal(err)
	}
	w.fail = true
	if _, err := rc.Enable(); err == nil {
		t.Fatal("enable succeeded despite writer failure")
	}
	got, ok := loadRemoteToken(rc.tokenPath)
	if !ok || got != prior {
		t.Fatalf("prior token not restored: %q %v", got, ok)
	}
	if rc.Running() {
		t.Fatal("listener left running after rollback")
	}
}

func TestRemoteStartup(t *testing.T) {
	rc, _ := newTestRemote(t, "127.0.0.1:0")
	tok, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, tok); err != nil {
		t.Fatal(err)
	}
	rc.Apply(config.Remote{Enabled: true})
	if !rc.Running() {
		t.Fatal("startup did not restore the listener")
	}

	for _, bad := range []string{"", "abc", strings.Repeat("z", 32)} {
		rc2, _ := newTestRemote(t, "127.0.0.1:0")
		if err := os.WriteFile(rc2.tokenPath, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		rc2.Apply(config.Remote{Enabled: true})
		if rc2.Running() {
			t.Fatalf("startup accepted token %q", bad)
		}
	}
}

func TestRemotePortOccupied(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	addr := blocker.Addr().String()

	rc, w := newTestRemote(t, addr)
	if _, err := rc.Enable(); err == nil {
		t.Fatal("enable succeeded on an occupied port")
	}
	if w.calls != 0 {
		t.Fatal("enable persisted config despite bind failure")
	}
	if _, ok := loadRemoteToken(rc.tokenPath); ok {
		t.Fatal("enable persisted a token despite bind failure")
	}
	// Startup with the port occupied: stays off, daemon unaffected.
	tok, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, tok); err != nil {
		t.Fatal(err)
	}
	rc.Apply(config.Remote{Enabled: true})
	if rc.Running() {
		t.Fatal("startup claims running on an occupied port")
	}
	// The port frees; enable now succeeds.
	deleteRemoteToken(rc.tokenPath)
	blocker.Close()
	if _, err := rc.Enable(); err != nil {
		t.Fatalf("enable after port freed: %v", err)
	}
}

func newTestServerWithRemote(t *testing.T, addr string) (*Server, *Remote) {
	t.Helper()
	h := newHarness(t)
	rc := NewRemote(h.server, &fakeRemoteWriter{}, filepath.Join(t.TempDir(), "remote-token"), addr, "desk")
	t.Cleanup(rc.Close)
	return h.server.WithRemote(rc), rc
}

func TestRemoteToggleRoute(t *testing.T) {
	s, rc := newTestServerWithRemote(t, "127.0.0.1:0")
	h := s.Handler()

	req := httptest.NewRequest("POST", "http://127.0.0.1/api/remote", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Declared bool     `json:"declared"`
		Running  bool     `json:"running"`
		URLs     []string `json:"urls"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Declared || !out.Running || len(out.URLs) == 0 || !rc.Running() {
		t.Fatalf("enable did not take: %+v", out)
	}
	req = httptest.NewRequest("POST", "http://127.0.0.1/api/remote", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rc.Running() || rc.Declared() {
		t.Fatalf("disable did not take: %d", rr.Code)
	}
}

// The toggle retracts a declaration whose listener never came up, because the
// UI drives from declared state rather than running state.
func TestRemoteToggleRetractsAfterFailedStartup(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	s, rc := newTestServerWithRemote(t, blocker.Addr().String())
	tok, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, tok); err != nil {
		t.Fatal(err)
	}
	rc.Apply(config.Remote{Enabled: true})

	req := httptest.NewRequest("POST", "http://127.0.0.1/api/remote", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rc.Declared() {
		t.Fatalf("toggle did not retract: %d declared=%v", rr.Code, rc.Declared())
	}
}

// Reload applies a hand-edited remote declaration to the live lifecycle:
// disabling stops the listener, and re-enabling with an address change
// rebinds. Without this, a reload adopted cfg.Remote and did nothing.
func TestReloadAppliesRemoteDeclaration(t *testing.T) {
	h := newHarness(t)
	rc := NewRemote(h.server, h.writer, filepath.Join(t.TempDir(), "remote-token"), "127.0.0.1:0", "desk")
	t.Cleanup(rc.Close)
	h.mgr.ReloadRemote = rc.Apply

	if _, err := rc.Enable(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	edit := func(remote string) {
		raw, err := os.ReadFile(h.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		doc["remote"] = json.RawMessage(remote)
		next, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.cfgPath, next, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	edit(`{"enabled":false,"addr":"127.0.0.1:0"}`)
	if _, err := h.mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rc.Running() || rc.Declared() {
		t.Fatal("reload did not stop the disabled listener")
	}

	tok, _ := newRemoteToken()
	if err := storeRemoteToken(rc.tokenPath, tok); err != nil {
		t.Fatal(err)
	}
	edit(`{"enabled":true,"addr":"127.0.0.1:0"}`)
	if _, err := h.mgr.Reload(); err != nil {
		t.Fatalf("reload enable: %v", err)
	}
	if !rc.Running() {
		t.Fatal("reload did not start the enabled listener")
	}
}

// Acceptance 1 of the polish spec: advertise round-trips through the toggle
// route, leads the URL list, and an invalid value persists nothing.
func TestRemoteToggleAdvertise(t *testing.T) {
	s, rc := newTestServerWithRemote(t, "127.0.0.1:0")
	h := s.Handler()
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1/api/remote", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := post(`{"advertise":"https://mcpd.home.example/"}`); rr.Code != http.StatusOK {
		t.Fatalf("set advertise: %d %s", rr.Code, rr.Body)
	}
	if got := rc.Advertise(); got != "https://mcpd.home.example" {
		t.Fatalf("advertise not adopted: %q", got)
	}
	rr := post(`{"enabled":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body)
	}
	var out struct {
		URLs      []string `json:"urls"`
		Advertise string   `json:"advertise"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.URLs) == 0 || !strings.HasPrefix(out.URLs[0], "https://mcpd.home.example/?token=") {
		t.Fatalf("advertised origin does not lead the URLs: %v", out.URLs)
	}
	if out.Advertise != "https://mcpd.home.example" {
		t.Fatalf("response advertise = %q", out.Advertise)
	}

	if rr := post(`{"advertise":"not an origin"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid advertise: %d %s", rr.Code, rr.Body)
	}
	if got := rc.Advertise(); got != "https://mcpd.home.example" {
		t.Fatalf("invalid advertise overwrote the good one: %q", got)
	}
	// An enabled-only request must not clear the advertise value.
	if rr := post(`{"enabled":false}`); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	if got := rc.Advertise(); got != "https://mcpd.home.example" {
		t.Fatalf("disable cleared advertise: %q", got)
	}
	// Empty string clears it.
	if rr := post(`{"advertise":""}`); rr.Code != http.StatusOK {
		t.Fatalf("clear advertise: %d", rr.Code)
	}
	if got := rc.Advertise(); got != "" {
		t.Fatalf("advertise not cleared: %q", got)
	}
}
