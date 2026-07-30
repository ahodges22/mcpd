package oauthstore_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	swap  *swapHandler

	lastCallback string
}

// swapHandler serves whichever web surface is current, so a restart can replace the
// daemon without moving the address its redirect URI is registered with. A real
// daemon keeps that address by binding a fixed port, and the provider refuses a
// redirect URI that does not match the registration.
type swapHandler struct{ current atomic.Pointer[http.Handler] }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.current.Load()).ServeHTTP(w, r)
}

func start(t *testing.T, dir string, p *provider, ts *httptest.Server, swap *swapHandler) *daemon {
	t.Helper()
	cfg := &config.Config{Backends: map[string]config.Backend{
		server: {Name: server, HTTPURL: p.mcpURL(), Auth: "oauth", TimeoutSec: 10},
	}}
	ov, err := backend.LoadOverrides(filepath.Join(dir, "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	d := &daemon{t: t, dir: dir, prov: p, web: ts, swap: swap}
	// The redirect URI is the address this server already answers on, rather than a
	// port a test assumed.
	d.store = oauthstore.New(dir, "http://"+ts.Listener.Addr().String()+"/oauth/callback",
		oauthstore.Hooks{
			NeedsAuth: func(name, note string) {
				if b, ok := d.reg.Get(name); ok {
					b.NoteNeedsAuth(note)
				}
			},
			Authorized: func(name string) {
				if b, ok := d.reg.Get(name); ok {
					b.NoteAuthorized()
				}
			},
		})
	d.reg = backend.NewRegistry(cfg, ov, backend.Hooks{
		Reconnected: func(s string) { d.cat.Trigger(s) },
		StopRefresh: func(s string) { d.cat.StopRefresh(s) },
		DropTools:   func(s string) { d.cat.Drop(s) },
		Refresh:     func(s string) { d.cat.Trigger(s) },
		AuthHandler: d.store.Handler,
	})
	d.cat = catalog.New(d.reg, filepath.Join(dir, "catalog.json"))
	surface := web.New(d.reg, d.cat, web.NewGuard(), d.store).Handler()
	swap.current.Store(&surface)
	return d
}

func newDaemon(t *testing.T, tools ...*mcp.Tool) *daemon {
	t.Helper()
	swap := &swapHandler{}
	// Unstarted first, so the store can be given the address this server will answer
	// on before anything is served through it.
	ts := httptest.NewUnstartedServer(swap)
	t.Cleanup(ts.Close)
	d := start(t, t.TempDir(), newProvider(t, testfake.New(server, tools...)), ts, swap)
	ts.Start()
	return d
}

// restart builds a fresh store, registry and web surface over the same state
// directory, the same provider and the same address, which is what a daemon restart
// looks like from the provider's side: the redirect URI it registered has not moved.
func (d *daemon) restart() *daemon { return start(d.t, d.dir, d.prov, d.web, d.swap) }

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
	d.rewriteRecord(func(rec *stored) { rec.Expiry = time.Now().Add(-time.Hour) })
}

// moveTokenEndpoint points the persisted token endpoint at a path the provider does not
// serve, which is what a provider moving its endpoint looks like to a restart. The
// refresh then fails with no OAuth error code at all.
func (d *daemon) moveTokenEndpoint() {
	d.t.Helper()
	d.rewriteRecord(func(rec *stored) { rec.TokenURL = d.prov.ts.URL + "/moved/token" })
}

func (d *daemon) rewriteRecord(mutate func(*stored)) {
	d.t.Helper()
	rec := d.stored()
	mutate(&rec)
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

// awaitPending returns the authorization URL the daemon published for the user,
// polling as the authenticate action does.
func (d *daemon) awaitPending() string {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if u, pending := d.store.Pending(server); pending {
			return u
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.t.Fatalf("no authorization was published for %s", server)
	return ""
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

// read fetches one of the daemon's own pages as the browser showing the status
// surface does, through the real guard and the real mux.
func (d *daemon) read(path string) response {
	d.t.Helper()
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodGet, d.web.URL+path, nil)
	if err != nil {
		d.t.Fatalf("build read: %v", err)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := d.web.Client().Do(req)
	if err != nil {
		d.t.Fatalf("read %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		d.t.Fatalf("read %s body: %v", path, err)
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

func TestTheStatusSurfaceCarriesTheTokenExpiryAndNoToken(t *testing.T) {
	// The status-ui requirement names token expiry among the fields the surface lists,
	// and it is the only part of the record that may leave the store.
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	rec := d.stored()
	expiry := rec.Expiry.Format(time.RFC3339)

	for _, path := range []string{"/api/status", "/"} {
		res := d.read(path)
		if res.status != http.StatusOK {
			t.Fatalf("GET %s = %d (%s), want %d", path, res.status, res.body, http.StatusOK)
		}
		if !strings.Contains(res.body, expiry) {
			t.Errorf("GET %s does not carry the token expiry %s: %s", path, expiry, res.body)
		}
		if strings.Contains(res.body, rec.AccessToken) || strings.Contains(res.body, rec.RefreshToken) {
			t.Errorf("GET %s carries token material", path)
		}
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

func TestAFailedRefreshStopsRatherThanLooping(t *testing.T) {
	// Both classes of refusal are driven. invalid_grant is the RFC code for a revoked
	// or expired grant and so the realistic one, and it is the dangerous one: the SDK
	// swallows it and sends the request unauthenticated, so the 401 that comes back
	// would re-enter the browser flow on every refresh attempt, parking a connect each
	// time. invalid_client stands for the class the SDK aborts the request on instead.
	for _, code := range []string{"invalid_grant", "invalid_client"} {
		t.Run(code, func(t *testing.T) {
			d := newDaemon(t, tool("search"))
			d.authorize(nil)
			d.expireAccessToken()
			d.prov.refuseRefresh(code)
			before := d.prov.counts()
			next := d.restart()

			// Twice, with the failure backoff cleared in between, because one read cannot
			// show a loop and an explicit reconnect must not stand in for the user asking.
			// The witness is the provider's metadata endpoint: an authorization attempt
			// begins with discovery, and a refused one does not reach it. Asserting on the
			// pending registry instead would prove nothing, because a synchronous read has
			// already withdrawn its own pending by the time it returns.
			for attempt := range 2 {
				if attempt > 0 {
					if err := next.reg.Reconnect(server); err != nil {
						t.Fatalf("reconnect: %v", err)
					}
				}
				if _, err := next.list(); err == nil {
					t.Fatalf("read %d: a backend whose refresh failed listed tools", attempt)
				}
				if n := next.prov.resourceMeta.Load(); n != before.resourceMeta {
					t.Fatalf("read %d started an authorization nobody asked for: metadata fetches = %d, want %d",
						attempt, n, before.resourceMeta)
				}
			}

			h := next.health()
			if h.State != backend.StateNeedsAuth {
				t.Errorf("state after a failed refresh = %q, want %q", h.State, backend.StateNeedsAuth)
			}
			if !strings.Contains(h.AuthNote, "authenticate action") {
				t.Errorf("auth note = %q, want it to point the user at the authenticate action", h.AuthNote)
			}

			// The block is on the daemon acting by itself, not on the user: asking must
			// still work, or a dead grant would be unrecoverable without an edit or a restart.
			status, payload := next.postAuthenticate()
			if status != http.StatusOK || payload["authorize_url"] == "" {
				t.Fatalf("authenticate action after a failed refresh = %d %v, want %d with a URL",
					status, payload, http.StatusOK)
			}
			if res := next.consentInBrowser(payload["authorize_url"]); res.status != http.StatusOK {
				t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
			}
			next.cat.WaitIdle()
			if got := next.health().State; got != backend.StateUp {
				t.Errorf("state after re-authorizing = %q, want %q", got, backend.StateUp)
			}
			if n := next.prov.registrations.Load(); n != before.registrations {
				t.Errorf("client registrations = %d, want %d: re-authorizing registered another client", n, before.registrations)
			}

			// The refusal must not outlive the grant it was about. With a new grant in
			// place, a later revocation gets an automatic authorization again rather than a
			// permanent demand for another click.
			next.prov.revokeAccessTokens()
			done := make(chan error, 1)
			go func() {
				_, err := next.list()
				done <- err
			}()
			if res := next.consentInBrowser(next.awaitPending()); res.status != http.StatusOK {
				t.Fatalf("callback for the automatic re-authorization = %d %q, want %d", res.status, res.body, http.StatusOK)
			}
			if err := <-done; err != nil {
				t.Fatalf("the read that should have re-authorized on its own failed: %v", err)
			}
		})
	}
}

func TestTheAuthenticateActionKeepsAStoredGrantThatStillWorks(t *testing.T) {
	// A restarted backend is down until its first connect, so a user who presses
	// Authenticate out of habit must not be given a consent screen for a grant that was
	// never refused. Discarding it would also leave the handler presenting nothing, so
	// every later read would publish a fresh authorization: the loop that test 10 exists
	// to prevent, reached through the recovery door.
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	before := d.prov.counts()
	next := d.restart()
	if got := next.health().State; got == backend.StateUp {
		t.Fatalf("state after restart = %q, want a backend that has not connected yet", got)
	}

	status, payload := next.postAuthenticate()
	if status != http.StatusOK || payload["status"] != "authorized" {
		t.Errorf("authenticate on a restarted backend = %d %v, want %d and status authorized",
			status, payload, http.StatusOK)
	}
	if got := next.prov.counts(); got != before {
		t.Errorf("provider counts = %+v, want %+v: the stored grant was discarded and a new one demanded", got, before)
	}
	if got := next.health().State; got != backend.StateUp {
		t.Errorf("state = %q, want %q: the stored grant no longer works", got, backend.StateUp)
	}

	// And nothing was left in a state where the daemon authorizes on its own.
	if _, err := next.list(); err != nil {
		t.Fatalf("list after the action: %v", err)
	}
	if got := next.prov.counts(); got.resourceMeta != before.resourceMeta {
		t.Errorf("metadata fetches = %d, want %d: a later read started an authorization",
			got.resourceMeta, before.resourceMeta)
	}
}

func TestATransientRefreshFailureDoesNotLatch(t *testing.T) {
	// A blip is not a dead grant. Latching on one would demand a click that the user
	// cannot usefully make, and would then refuse the automatic re-authorization that a
	// genuine 401 is supposed to get.
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	d.expireAccessToken()
	d.prov.refuseRefresh("temporarily_unavailable")
	next := d.restart()

	if _, err := next.list(); err == nil {
		t.Fatal("a backend whose refresh failed listed tools")
	}
	if got := next.health().State; got != backend.StateDown {
		t.Errorf("state after a transient refusal = %q, want %q: a blip is not a rejected grant", got, backend.StateDown)
	}

	next.prov.allowRefresh()
	if err := next.reg.Reconnect(server); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := next.list(); err != nil {
		t.Fatalf("list after the provider recovered: %v", err)
	}
	if got := next.health().State; got != backend.StateUp {
		t.Fatalf("state after the provider recovered = %q, want %q", got, backend.StateUp)
	}

	// The recovered grant must not still be blocked: a genuine 401 has to be able to
	// authorize automatically, which is the whole of test 13's scenario.
	next.prov.revokeAccessTokens()
	done := make(chan error, 1)
	go func() {
		_, err := next.list()
		done <- err
	}()
	if res := next.consentInBrowser(next.awaitPending()); res.status != http.StatusOK {
		t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
	}
	if err := <-done; err != nil {
		t.Fatalf("the read that triggered re-authorization failed: %v", err)
	}
	if got := next.health().State; got != backend.StateUp {
		t.Errorf("state after re-authorizing = %q, want %q", got, backend.StateUp)
	}
}

func TestARefusalTheClassifierDoesNotRecogniseStillRecoversInOneClick(t *testing.T) {
	// Only invalid_grant and invalid_client are treated as permanent, because that
	// judgement decides whether a dial may authorize by itself. Recovery must not
	// inherit it: any refusal at all has to be recoverable from the button, in one
	// click, without the user first being made to wait out the window.
	for _, staging := range []string{"an unrecognised refusal code", "a moved token endpoint"} {
		t.Run(staging, func(t *testing.T) {
			d := newDaemon(t, tool("search"))
			d.authorize(nil)
			d.expireAccessToken()
			if staging == "a moved token endpoint" {
				d.moveTokenEndpoint()
			} else {
				d.prov.refuseRefresh("unauthorized_client")
			}
			before := d.prov.counts()
			next := d.restart()

			if _, err := next.list(); err == nil {
				t.Fatal("a backend whose stored grant is unusable listed tools")
			}
			// Deliberately not needs-auth: the refusal is not one that can reach a 401, so
			// the automatic paths are left alone and nothing latches.
			if got := next.health().State; got != backend.StateDown {
				t.Errorf("state = %q, want %q", got, backend.StateDown)
			}

			start := time.Now()
			status, payload := next.postAuthenticate()
			if status != http.StatusOK || payload["authorize_url"] == "" {
				t.Fatalf("one click = %d %v, want %d with a URL", status, payload, http.StatusOK)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("the click took %s, want it to answer as soon as the reconnect has failed", took)
			}
			if res := next.consentInBrowser(payload["authorize_url"]); res.status != http.StatusOK {
				t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
			}
			next.cat.WaitIdle()
			if got := next.health().State; got != backend.StateUp {
				t.Errorf("state after re-authorizing = %q, want %q", got, backend.StateUp)
			}
			if n := next.prov.registrations.Load(); n != before.registrations {
				t.Errorf("client registrations = %d, want %d: recovery registered another client", n, before.registrations)
			}
		})
	}
}

func TestEachAuthorizationAttemptGetsItsOwnWaitBudget(t *testing.T) {
	// Lowered rather than staged in real time: the shipped 30s value is what the daemon
	// runs with, and this test's subject is how the budget is shared, not its size.
	restore := web.AuthorizeWaitTimeout
	web.AuthorizeWaitTimeout = 2 * time.Second
	t.Cleanup(func() { web.AuthorizeWaitTimeout = restore })

	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	d.expireAccessToken()
	// A refusal that surfaces after one attempt's budget: under a shared deadline the
	// first attempt spends all of it and the second has none left to discover a URL in.
	d.prov.refuseRefresh("unauthorized_client")
	d.prov.delayNextRefresh(web.AuthorizeWaitTimeout + 500*time.Millisecond)
	next := d.restart()

	status, payload := next.postAuthenticate()
	if status != http.StatusOK || payload["authorize_url"] == "" {
		t.Fatalf("one click after a refusal slower than one attempt's budget = %d %v with health %+v, want %d with a URL",
			status, payload, next.health(), http.StatusOK)
	}
}

func TestASecondAuthorizationReusesTheRegisteredClient(t *testing.T) {
	d := newDaemon(t, tool("search"))
	d.authorize(nil)
	first := d.stored()

	// The provider forgets the access token it issued, which is a consent withdrawn
	// there. The token is unexpired, so nothing refreshes and nothing is blocked: the
	// next request on the live session simply 401s, and the authorization that follows
	// runs on the handler the first one built rather than on a fresh process.
	d.prov.revokeAccessTokens()
	done := make(chan error, 1)
	go func() {
		_, err := d.list()
		done <- err
	}()
	if res := d.consentInBrowser(d.awaitPending()); res.status != http.StatusOK {
		t.Fatalf("callback = %d %q, want %d", res.status, res.body, http.StatusOK)
	}
	if err := <-done; err != nil {
		t.Fatalf("the read that triggered the second authorization failed: %v", err)
	}

	if n := d.prov.registrations.Load(); n != 1 {
		t.Errorf("client registrations = %d, want 1: the persisted client_id was not reused, so a client is orphaned at the provider", n)
	}
	if got := d.stored(); got.ClientID != first.ClientID {
		t.Errorf("client_id = %q, want the persisted %q", got.ClientID, first.ClientID)
	}
	if n := d.prov.exchanges.Load(); n != 2 {
		t.Errorf("code exchanges = %d, want 2: the second authorization did not complete", n)
	}
	if got := d.health().State; got != backend.StateUp {
		t.Errorf("state after re-authorizing = %q, want %q", got, backend.StateUp)
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

	// Pressing it again on a backend that is up must answer from the health record.
	// The button is rendered for every OAuth backend whatever its state, so this is a
	// routine click: it must not drop a working session to discover that nothing was
	// needed, nor wait out the window an authorization would have appeared in.
	before, requests := d.prov.counts(), d.prov.requests()
	start := time.Now()
	status, payload = d.postAuthenticate()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("authenticate on an authorized backend took %s, want an immediate answer", took)
	}
	if status != http.StatusOK || payload["status"] != "authorized" {
		t.Errorf("authenticate on an authorized backend = %d %v, want %d and status authorized",
			status, payload, http.StatusOK)
	}
	if got := d.prov.counts(); got != before {
		t.Errorf("provider counts = %+v, want %+v: it re-authorized a backend that was up", got, before)
	}
	if n := d.prov.requests(); n != requests {
		t.Errorf("MCP requests = %d, want %d: the live session was torn down and re-established", n, requests)
	}
	if got := d.health().State; got != backend.StateUp {
		t.Errorf("state = %q, want %q", got, backend.StateUp)
	}

	// A click during a handshake must wait for its own reconnect, not mistake the
	// interrupted handshake for a failed attempt and discard a working grant.
	next := d.restart()
	started := next.prov.blockNextMCPRequest()
	read := make(chan error, 1)
	go func() {
		_, err := next.list()
		read <- err
	}()
	<-started
	before = next.prov.counts()
	status, payload = next.postAuthenticate()
	if status != http.StatusOK || payload["status"] != "authorized" {
		t.Errorf("authenticate during a handshake = %d %v, want %d and status authorized", status, payload, http.StatusOK)
	}
	if got := next.prov.counts(); got.authorizations != before.authorizations {
		t.Errorf("authorizations = %d, want %d: the click discarded a working grant", got.authorizations, before.authorizations)
	}
	if err := <-read; err == nil {
		t.Error("the reconnect did not interrupt the in-flight read")
	}
	if got := next.health().State; got != backend.StateUp {
		t.Errorf("state after the reconnect = %q, want %q", got, backend.StateUp)
	}

	// A later lifecycle transition supersedes this click instead of rejecting its grant.
	superseded := next.restart()
	started = superseded.prov.blockNextMCPRequest()
	clicked := make(chan int, 1)
	go func() {
		status, _ := superseded.postAuthenticate()
		clicked <- status
	}()
	<-started
	if err := superseded.reg.Disable(server); err != nil {
		t.Fatalf("disable during authenticate: %v", err)
	}
	if got := <-clicked; got != http.StatusConflict {
		t.Errorf("authenticate superseded by a disable = %d, want %d", got, http.StatusConflict)
	}
	if err := superseded.reg.Enable(server); err != nil {
		t.Fatalf("enable after authenticate: %v", err)
	}
	if _, err := superseded.list(); err != nil {
		t.Errorf("list after the superseding lifecycle transition: %v", err)
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
