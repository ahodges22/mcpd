package oauthstore_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/oauthstore"
	"github.com/ahodges/mcpd/internal/testfake"
	"github.com/ahodges/mcpd/internal/web"
)

// server is the name of the one OAuth-gated backend these tests declare.
const server = "notion"

// daemon is one run of the parts an OAuth-gated backend needs: the store, the
// registry that dials through it, and the real loopback web surface the provider
// redirects the browser to. Building a second one over the same directory is a
// restart.
type daemon struct {
	t     *testing.T
	dir   string
	prov  *provider
	store *oauthstore.Store
	reg   *backend.Registry
	cat   *catalog.Catalog
	web   *httptest.Server

	lastCallback string
}

func start(t *testing.T, dir string, p *provider) *daemon {
	t.Helper()
	cfg := &config.Config{Backends: map[string]config.Backend{
		server: {Name: server, HTTPURL: p.mcpURL(), Auth: "oauth", TimeoutSec: 10},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(dir, "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	d := &daemon{t: t, dir: dir, prov: p}
	// Unstarted, so the redirect URI is the address this very server answers on
	// rather than a port a test assumed.
	d.web = httptest.NewUnstartedServer(nil)
	d.store = oauthstore.New(dir, "http://"+d.web.Listener.Addr().String()+"/oauth/callback",
		oauthstore.Hooks{NeedsAuth: func(name, note string) {
			if b, ok := d.reg.Get(name); ok {
				b.NoteNeedsAuth(note)
			}
		}})
	d.reg = backend.NewRegistry(cfg, ov, backend.Hooks{
		Reconnected: func(s string) { d.cat.Trigger(s) },
		StopRefresh: func(s string) { d.cat.StopRefresh(s) },
		DropTools:   func(s string) { d.cat.Drop(s) },
		Refresh:     func(s string) { d.cat.Trigger(s) },
		AuthHandler: d.store.Handler,
	})
	d.cat = catalog.New(d.reg, filepath.Join(dir, "catalog.json"))
	d.web.Config.Handler = web.New(d.reg, d.cat, web.NewGuard(), d.store).Handler()
	d.web.Start()
	t.Cleanup(d.web.Close)
	return d
}

func newDaemon(t *testing.T, tools ...*mcp.Tool) *daemon {
	t.Helper()
	return start(t, t.TempDir(), newProvider(t, testfake.New(server, tools...)))
}

// restart builds a fresh store, registry and web surface over the same state
// directory and the same provider, which is what a daemon restart looks like from
// the provider's side.
func (d *daemon) restart() *daemon { return start(d.t, d.dir, d.prov) }

func (d *daemon) list() ([]*mcp.Tool, error) {
	d.t.Helper()
	b, ok := d.reg.Get(server)
	if !ok {
		d.t.Fatalf("backend %q is not registered", server)
	}
	return b.ListTools(d.t.Context())
}

func (d *daemon) health() backend.Health { return d.reg.Health()[server] }

func (d *daemon) tokenPath() string {
	return filepath.Join(d.dir, "oauth-"+server+".json")
}

// stored is the persisted shape, declared here rather than shared with the package
// so a test would notice the on-disk format changing under it.
type stored struct {
	ClientID     string    `json:"client_id"`
	Issuer       string    `json:"issuer"`
	TokenURL     string    `json:"token_url"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
}

func (d *daemon) stored() stored {
	d.t.Helper()
	raw, err := os.ReadFile(d.tokenPath())
	if err != nil {
		d.t.Fatalf("read token file: %v", err)
	}
	var rec stored
	if err := json.Unmarshal(raw, &rec); err != nil {
		d.t.Fatalf("parse token file: %v", err)
	}
	return rec
}

// expireAccessToken ages the persisted access token, which is what an elapsed hour
// leaves behind: a dead access token and a live refresh token.
func (d *daemon) expireAccessToken() {
	d.t.Helper()
	rec := d.stored()
	rec.Expiry = time.Now().Add(-time.Hour)
	raw, err := json.Marshal(rec)
	if err != nil {
		d.t.Fatalf("marshal token file: %v", err)
	}
	if err := os.WriteFile(d.tokenPath(), raw, 0o600); err != nil {
		d.t.Fatalf("write token file: %v", err)
	}
}

func (d *daemon) noTokenFile() bool {
	_, err := os.Stat(d.tokenPath())
	return os.IsNotExist(err)
}

// awaitPending returns the authorization URL the daemon published for the user.
func (d *daemon) awaitPending() string {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(d.t.Context(), 15*time.Second)
	defer cancel()
	u, err := d.store.Await(ctx, server)
	if err != nil {
		d.t.Fatalf("no authorization was published for %s: %v", server, err)
	}
	return u
}

// authorize runs a first-time authorization to completion the way the user does:
// a read 401s and parks, the daemon publishes a URL, and the browser completes the
// consent and the callback. inspect, when non-nil, runs while the authorization is
// still pending.
func (d *daemon) authorize(inspect func(authURL string)) {
	d.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := d.list()
		done <- err
	}()
	authURL := d.awaitPending()
	if inspect != nil {
		inspect(authURL)
	}
	if res := d.consentInBrowser(authURL); res.status != http.StatusOK {
		d.t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
	}
	if err := <-done; err != nil {
		d.t.Fatalf("the read that triggered authorization failed: %v", err)
	}
}

// consentInBrowser visits the provider's authorization endpoint and then follows
// its redirect to the daemon, as the user's browser would.
func (d *daemon) consentInBrowser(authURL string) response {
	d.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodGet, authURL, nil)
	if err != nil {
		d.t.Fatalf("build consent request: %v", err)
	}
	res, err := client.Do(req)
	if err != nil {
		d.t.Fatalf("consent: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusFound {
		d.t.Fatalf("provider consent = %d %q, want %d", res.StatusCode, body, http.StatusFound)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, d.web.URL+"/oauth/callback") {
		d.t.Fatalf("provider redirected to %q, want the daemon's callback under %s", loc, d.web.URL)
	}
	d.lastCallback = loc
	return d.navigate(loc)
}

type response struct {
	status int
	body   string
}

// navigate performs a top-level browser navigation through the real guard and the
// real mux. The headers are what a browser sends once a provider redirects it to a
// loopback address: the navigation is cross-site and carries no JSON content type,
// so the state nonce is the only thing standing behind this route.
func (d *daemon) navigate(target string) response {
	d.t.Helper()
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodGet, target, nil)
	if err != nil {
		d.t.Fatalf("build navigation: %v", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	res, err := d.web.Client().Do(req)
	if err != nil {
		d.t.Fatalf("navigate to %s: %v", target, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		d.t.Fatalf("read navigation body: %v", err)
	}
	return response{status: res.StatusCode, body: string(body)}
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: name + " description",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}}}`),
	}
}

func TestAFirstAuthorizationBringsTheBackendUp(t *testing.T) {
	d := newDaemon(t, tool("search"))

	d.authorize(func(authURL string) {
		// The spec's needs-auth scenario: while the authorization is outstanding the
		// state says so and the URL the user must visit is available.
		h := d.health()
		if h.State != backend.StateNeedsAuth {
			t.Errorf("state while pending = %q, want %q", h.State, backend.StateNeedsAuth)
		}
		if !strings.Contains(h.AuthNote, authURL) {
			t.Errorf("auth note = %q, want it to carry %q", h.AuthNote, authURL)
		}
	})

	if got := d.health().State; got != backend.StateUp {
		t.Errorf("state after authorization = %q, want %q", got, backend.StateUp)
	}
	if got := d.health().AuthNote; got != "oauth" {
		t.Errorf("auth note after authorization = %q, want the pending URL to be gone", got)
	}
	if n := d.prov.challenges.Load(); n == 0 {
		t.Error("the provider never issued a 401 challenge, so discovery was not driven by one")
	}
	if n := d.prov.resourceMeta.Load(); n == 0 {
		t.Error("protected-resource metadata was never fetched")
	}
	if n := d.prov.registrations.Load(); n != 1 {
		t.Errorf("dynamic client registrations = %d, want 1", n)
	}
	if n := d.prov.exchanges.Load(); n != 1 {
		t.Errorf("code exchanges = %d, want 1", n)
	}

	rec := d.stored()
	// The issuer can only come from the challenge's resource_metadata pointer, so
	// finding it here is evidence the challenge was read rather than a path guessed.
	if rec.Issuer != d.prov.ts.URL {
		t.Errorf("stored issuer = %q, want %q", rec.Issuer, d.prov.ts.URL)
	}
	if rec.TokenURL != d.prov.ts.URL+"/token" {
		t.Errorf("stored token endpoint = %q, want %q", rec.TokenURL, d.prov.ts.URL+"/token")
	}
	if rec.ClientID == "" || rec.AccessToken == "" || rec.RefreshToken == "" || rec.Expiry.IsZero() {
		t.Errorf("stored record is incomplete: %+v", redact(rec))
	}
}

func TestTheTokenFileIsPrivate(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)

	info, err := os.Stat(d.tokenPath())
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("token file mode = %#o, want %#o", got, 0o600)
	}
	dir, err := os.Stat(d.dir)
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("state directory mode = %#o, want %#o", got, 0o700)
	}
}

func TestAReconnectStaysAuthorized(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	before := d.prov.counts()
	served := d.prov.bearerServed.Load()

	if err := d.reg.Reconnect(server); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	tools, err := d.list()
	if err != nil {
		t.Fatalf("list after reconnect: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("tools after reconnect = %d, want 1", len(tools))
	}
	if got := d.prov.counts(); got != before {
		t.Errorf("provider counts after reconnect = %+v, want %+v: the reconnect re-authorized", got, before)
	}
	if n := d.prov.bearerServed.Load(); n <= served {
		t.Errorf("authenticated requests = %d, want more than %d: the reconnect carried no token", n, served)
	}
}

func TestARestartReusesTheStoredToken(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	before := d.prov.counts()

	next := d.restart()
	tools, err := next.list()
	if err != nil {
		t.Fatalf("list after restart: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("tools after restart = %d, want 1", len(tools))
	}
	// No new challenge means the very first request after the restart already carried
	// the stored token, and no new registration means the stored client_id was reused.
	if got := next.prov.counts(); got != before {
		t.Errorf("provider counts after restart = %+v, want %+v", got, before)
	}
	if got := next.health().State; got != backend.StateUp {
		t.Errorf("state after restart = %q, want %q", got, backend.StateUp)
	}
}

func TestAnExpiredAccessTokenRefreshes(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	first := d.stored()
	d.expireAccessToken()
	before := d.prov.counts()

	next := d.restart()
	if _, err := next.list(); err != nil {
		t.Fatalf("list with an expired access token: %v", err)
	}
	if n := next.prov.refreshes.Load() - before.refreshes; n != 1 {
		t.Errorf("refresh grants = %d, want 1", n)
	}
	if n := next.prov.authorizations.Load(); n != before.authorizations {
		t.Errorf("authorization requests = %d, want %d: the refresh went through the browser", n, before.authorizations)
	}

	second := next.stored()
	if second.AccessToken == first.AccessToken {
		t.Error("the refreshed access token was not written back")
	}
	if second.RefreshToken == "" || !second.Expiry.After(time.Now()) {
		t.Errorf("written-back record is unusable: %+v", redact(second))
	}
	if second.ClientID != first.ClientID || second.TokenURL != first.TokenURL || second.Issuer != first.Issuer {
		t.Errorf("the write-back lost the client registration: %+v", redact(second))
	}
}

func TestAForgedCallbackIsRefused(t *testing.T) {
	d := newDaemon(t, tool("search"))
	done := make(chan error, 1)
	go func() {
		_, err := d.list()
		done <- err
	}()
	authURL := d.awaitPending()

	forged := d.web.URL + "/oauth/callback?state=forged&code=stolen"
	if res := d.navigate(forged); res.status != http.StatusBadRequest {
		t.Errorf("forged callback = %d %q, want %d", res.status, res.body, http.StatusBadRequest)
	}
	if !d.noTokenFile() {
		t.Error("a forged callback wrote a token file")
	}
	if n := d.prov.exchanges.Load(); n != 0 {
		t.Errorf("code exchanges after a forged callback = %d, want 0", n)
	}

	// The genuine callback still completes, so the absent token file was the guard's
	// work rather than a flow that could never have written one.
	if res := d.consentInBrowser(authURL); res.status != http.StatusOK {
		t.Fatalf("genuine callback = %d %q, want %d", res.status, res.body, http.StatusOK)
	}
	if err := <-done; err != nil {
		t.Fatalf("the read that triggered authorization failed: %v", err)
	}
	if d.noTokenFile() {
		t.Error("the genuine callback wrote no token file")
	}
}

func TestAReplayedCallbackIsRefused(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	first := d.stored()
	before := d.prov.counts()

	if res := d.navigate(d.lastCallback); res.status != http.StatusBadRequest {
		t.Errorf("replayed callback = %d %q, want %d: the state was not consumed", res.status, res.body, http.StatusBadRequest)
	}
	if n := d.prov.exchanges.Load(); n != before.exchanges {
		t.Errorf("code exchanges after a replay = %d, want %d", n, before.exchanges)
	}
	if got := d.stored(); got != first {
		t.Errorf("the replay rewrote the token file: %+v", redact(got))
	}
}

func TestA403NeitherAuthorizesNorRetries(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.prov.forbid.Store(true)

	if _, err := d.list(); err == nil {
		t.Fatal("a 403 backend listed tools")
	}
	if got := d.prov.counts(); got.authorizations != 0 || got.registrations != 0 || got.exchanges != 0 {
		t.Errorf("a 403 started the authorization flow: %+v", got)
	}
	if n := d.prov.resourceMeta.Load(); n != 0 {
		t.Errorf("a 403 started discovery: %d metadata fetches", n)
	}
	// A retry is what the SDK does after an Authorize that returns nil, and it would
	// show up as the same JSON-RPC method arriving twice.
	if repeated := d.prov.repeated(); len(repeated) != 0 {
		t.Errorf("methods sent more than once = %v, want none: the 403 was retried", repeated)
	}
	if got := d.health().State; got != backend.StateDown {
		t.Errorf("state after a 403 = %q, want %q", got, backend.StateDown)
	}
}

func TestADisableWhileAuthorizationIsPendingReturnsPromptly(t *testing.T) {
	d := newDaemon(t, tool("search"))
	done := make(chan error, 1)
	go func() {
		_, err := d.list()
		done <- err
	}()
	d.awaitPending()

	// Disabled on another goroutine and waited for with a deadline: a kill switch
	// that cannot interrupt the wait would otherwise park this test for the whole
	// pending window instead of failing.
	disabled := make(chan error, 1)
	go func() { disabled <- d.reg.Disable(server) }()
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disable did not return within 3s while an authorization was pending: it cannot interrupt the wait")
	}
	if err := <-done; err == nil {
		t.Error("the read succeeded although its authorization was abandoned")
	}
	if got := d.health().State; got != backend.StateDisabled {
		t.Errorf("state after disable = %q, want %q", got, backend.StateDisabled)
	}
}

func TestAFailedRefreshReturnsTheBackendToNeedsAuth(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	d.expireAccessToken()
	d.prov.rejectRefresh.Store(true)
	before := d.prov.counts()

	next := d.restart()
	if _, err := next.list(); err == nil {
		t.Fatal("a backend whose refresh failed listed tools")
	}
	h := next.health()
	if h.State != backend.StateNeedsAuth {
		t.Errorf("state after a failed refresh = %q, want %q", h.State, backend.StateNeedsAuth)
	}
	if !strings.Contains(h.AuthNote, "refresh failed") {
		t.Errorf("auth note = %q, want it to say the refresh failed", h.AuthNote)
	}
	if n := next.prov.authorizations.Load(); n != before.authorizations {
		t.Errorf("authorization requests = %d, want %d: the failed refresh looped into a new flow", n, before.authorizations)
	}
}

func TestTheAuthenticateActionStartsAnAuthorizationTheUserCanFinish(t *testing.T) {
	d := newDaemon(t, tool("search"))

	// The action answers only once there is somewhere to send the user, which is what
	// makes it usable from the status page: it reconnects, so a backend sitting in its
	// failure backoff still reaches the 401, and then waits out discovery.
	status, payload := d.postAuthenticate()
	if status != http.StatusOK {
		t.Fatalf("authenticate action = %d %v, want %d", status, payload, http.StatusOK)
	}
	authURL := payload["authorize_url"]
	if authURL == "" {
		t.Fatalf("the authenticate action returned no authorization URL: %v", payload)
	}
	if h := d.health(); h.State != backend.StateNeedsAuth {
		t.Errorf("state while pending = %q, want %q", h.State, backend.StateNeedsAuth)
	}

	if res := d.consentInBrowser(authURL); res.status != http.StatusOK {
		t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
	}
	// The read parked in the code fetcher belongs to the refresh this action
	// triggered, so waiting for that loop to finish is what waiting for the flow is.
	d.cat.WaitIdle()

	if got := d.health().State; got != backend.StateUp {
		t.Errorf("state after the callback = %q, want %q", got, backend.StateUp)
	}
	if n := len(d.cat.Entries()); n != 1 {
		t.Errorf("catalog entries = %d, want 1: the authorized backend served no tools", n)
	}
	if n := d.prov.registrations.Load(); n != 1 {
		t.Errorf("dynamic client registrations = %d, want 1", n)
	}
}

// postAuthenticate drives the status page's authenticate action the way its button
// does: a JSON POST, whose body the page discards when it reloads but a command
// line caller reads.
func (d *daemon) postAuthenticate() (int, map[string]string) {
	d.t.Helper()
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodPost,
		d.web.URL+"/api/backends/"+server+"/authorize", strings.NewReader(`{}`))
	if err != nil {
		d.t.Fatalf("build the authenticate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.web.Client().Do(req)
	if err != nil {
		d.t.Fatalf("authenticate: %v", err)
	}
	defer res.Body.Close()
	var payload map[string]string
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		d.t.Fatalf("decode the authenticate response: %v", err)
	}
	return res.StatusCode, payload
}

// redact keeps a failure message from carrying credentials into a test log.
func redact(rec stored) stored {
	if rec.AccessToken != "" {
		rec.AccessToken = "(set)"
	}
	if rec.RefreshToken != "" {
		rec.RefreshToken = "(set)"
	}
	return rec
}
