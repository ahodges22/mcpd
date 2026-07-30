// Package oauthstore owns mcpd's downstream OAuth: the token it holds per
// OAuth-gated backend, the authorizations awaiting a browser callback, and the
// handler a backend's HTTP transport authorizes with.
package oauthstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const (
	// tokenFileMode and stateDirMode are applied with Chmod rather than trusted to
	// the create call: the umask clears bits from a create mode, and this is the one
	// credential mcpd owns.
	tokenFileMode = 0o600
	stateDirMode  = 0o700
	// providerTimeout bounds one round trip to the provider: discovery,
	// registration, the code exchange, and every later refresh. Without it a hung
	// token endpoint parks a refresh, and with it every request that needs a token.
	providerTimeout = 30 * time.Second
	// clientName is what the provider shows on the consent screen.
	clientName = "mcpd"
	// needsUserNote is the auth note for a grant the provider rejected. The
	// provider's own reason reaches the status page as the backend's last error; this
	// is the part the user can act on.
	needsUserNote = "the stored authorization was rejected, use the authenticate action"
)

// Hooks are the backend transitions the store drives.
type Hooks struct {
	// NeedsAuth moves a backend to needs-auth, with note rendered as its auth note.
	// It is called while an authorization waits on the user, and when a token
	// refresh has failed so the user must authorize again.
	NeedsAuth func(server, note string)
	// Authorized clears that marking once an authorization has succeeded. It matters
	// because an authorization can complete on a session that never went down, and
	// then no handshake runs to record that the backend is serving again.
	Authorized func(server string)
}

// Store keeps one token file per backend under a directory the daemon owns. The
// user's configuration is never written: it declares backends and the daemon only
// reads it.
type Store struct {
	dir         string
	redirectURL string
	hooks       Hooks
	client      *http.Client

	mu       sync.Mutex
	handlers map[string]auth.OAuthHandler
	pending  map[string]*pending
	// grants records what is known against a backend's stored grant, which is what
	// decides whether a 401 may start a browser authorization by itself.
	grants map[string]grantState
}

// New returns a store persisting under dir and bridging the authorization flow
// through redirectURL, which must be the daemon's own callback route: it is
// registered with the provider as the redirect URI and must not move.
func New(dir, redirectURL string, hooks Hooks) *Store {
	return &Store{
		dir:         dir,
		redirectURL: redirectURL,
		hooks:       hooks,
		client:      &http.Client{Timeout: providerTimeout, Transport: publicClient{http.DefaultTransport}},
		handlers:    make(map[string]auth.OAuthHandler),
		pending:     make(map[string]*pending),
		grants:      make(map[string]grantState),
	}
}

// grantState is what the store knows about a backend's stored grant. The two facts
// are independent: whether the provider has refused the grant decides whether it is
// still worth presenting, and whether the user has asked decides whether an
// authorization may start.
type grantState struct {
	rejected  bool
	requested bool
}

// AllowAuthorization records that the user has asked server to be authorized, which
// is the only thing that lets an authorization start once the provider has refused
// the stored grant. A refused grant must wait for a person: re-entering the browser
// flow on the daemon's own refresh cadence publishes an authorization nobody asked
// for and parks a connect on every attempt.
func (s *Store) AllowAuthorization(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	g.requested = true
	s.grants[server] = g
	if g.rejected {
		// Only a grant the provider has actually refused is withdrawn from the next
		// dial: a request that cannot obtain a token is never sent, so it never reaches
		// the 401 an authorization starts from. A grant that may still work is left in
		// place, because forcing a consent screen on a backend that is merely down
		// discards a working authorization.
		delete(s.handlers, server)
	}
}

// rejectGrant records that the provider refused this backend's grant. It leaves a
// request the user has already made intact, so a refresh failure on the session being
// torn down cannot cancel the authorization they asked for.
func (s *Store) rejectGrant(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	g.rejected = true
	s.grants[server] = g
}

// grantWorks records that the provider has honoured the grant, which is what keeps a
// transient refusal from outliving itself. Without it one network blip would block
// automatic re-authorization for the life of the process.
func (s *Store) grantWorks(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, server)
}

// permitAuthorization reports whether a browser authorization may start now,
// consuming the user's request if there is one.
func (s *Store) permitAuthorization(server string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	if g.requested {
		g.requested = false
		s.grants[server] = g
		return true
	}
	return !g.rejected
}

// publicClient refuses to send a client-secret Basic header, which is what keeps an
// authorization code from being presented to the provider twice.
//
// mcpd registers with token_endpoint_auth_method "none" and holds no secret, so such
// a header can only come from oauth2's auth-style probe. That probe is reached
// whenever a client_id is preregistered and the authorization server advertises
// neither client_secret_post nor client_secret_basic, which is the normal case for a
// public client, and it sends the token request twice: Basic header first, form
// parameters second. Letting the first attempt reach the provider presents the code
// twice, and a provider that invalidates a code on first presentation then refuses
// the attempt that would have worked. Failing it here costs nothing and makes oauth2
// fall through to the form-parameter style by itself.
type publicClient struct{ base http.RoundTripper }

func (t publicClient) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, _, ok := req.BasicAuth(); ok {
		return nil, errors.New("mcpd is a public oauth client and sends no client secret")
	}
	return t.base.RoundTrip(req)
}

// record is one backend's persisted OAuth state. The client_id is kept because
// re-registering on every start would churn clients at the provider, and the
// issuer and token endpoint are kept so a restart can restore a token source
// without a discovery round trip.
type record struct {
	ClientID     string    `json:"client_id"`
	Issuer       string    `json:"issuer,omitempty"`
	TokenURL     string    `json:"token_url"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// Handler returns the authorization handler for server, built on first use from
// whatever has been persisted. The same wrapper is returned every time, so the
// transport of a reconnected session keeps a stable reference; the SDK handler
// inside it is rebuilt when a newly registered client_id has to be picked up.
func (s *Store) Handler(server string) (auth.OAuthHandler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.handlers[server]; ok {
		return h, nil
	}
	rec, err := s.load(server)
	if err != nil {
		return nil, err
	}
	h := &handler{store: s, server: server}
	if rec != nil {
		h.registeredAs = rec.ClientID
	}
	// A grant the user has just asked to replace is not presented: doing so is what
	// keeps a dead token source from failing every request before one can 401.
	// A grant the provider has refused is not presented: a token source that cannot
	// produce a token fails every request before one can 401, and the 401 is what an
	// authorization starts from.
	inner, err := s.newAuthorizer(h, rec, !s.grants[server].rejected)
	if err != nil {
		return nil, err
	}
	h.inner = inner
	s.handlers[server] = h
	return h, nil
}

// newAuthorizer builds one SDK authorization handler over rec, which may be nil on
// a first run. It is the only construction site, so the first-run path and the
// rebuild that picks up a registration cannot drift apart. restoreToken decides
// whether the persisted token is offered as the initial token source, which is what
// makes a restart reuse it.
func (s *Store) newAuthorizer(h *handler, rec *record, restoreToken bool) (*auth.AuthorizationCodeHandler, error) {
	cfg := &auth.AuthorizationCodeHandlerConfig{
		// Both registration methods are configured. The SDK prefers preregistration,
		// so a persisted client_id is reused and dynamic registration happens only on
		// a first run.
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:    clientName,
				RedirectURIs:  []string{s.redirectURL},
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"},
				// No secret to hold: a loopback redirect cannot keep one, and the
				// provider issues none for this method.
				TokenEndpointAuthMethod: "none",
			},
		},
		RedirectURL:              s.redirectURL,
		AuthorizationCodeFetcher: s.fetchCode(h.server),
		RequestRefreshToken:      true,
		Client:                   s.client,
		NewTokenSource: func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			src := s.persisting(h.server, record{
				ClientID: oc.ClientID,
				Issuer:   h.issuerSeen(),
				TokenURL: oc.Endpoint.TokenURL,
			}, oc.TokenSource(ctx, tok), nil)
			// Written here rather than left to the first refresh: a grant the user has
			// just completed must survive a restart.
			if err := src.persist(tok); err != nil {
				return nil, err
			}
			return src, nil
		},
	}
	if rec != nil {
		if rec.ClientID != "" {
			// Configured alongside dynamic registration, which the SDK only falls back to
			// when this is absent, so a persisted client_id is reused rather than a second
			// client being registered at the provider.
			cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: rec.ClientID, Issuer: rec.Issuer}
		}
		if restoreToken {
			cfg.InitialTokenSource = s.restore(h.server, *rec)
		}
	}
	inner, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, fmt.Errorf("oauth handler for %s: %w", h.server, err)
	}
	return inner, nil
}

// restore builds the token source a restart authorizes with. Without it the first
// request 401s and the whole browser flow runs again, so a persisted token would
// never be used.
func (s *Store) restore(server string, rec record) oauth2.TokenSource {
	tok := &oauth2.Token{
		AccessToken:  rec.AccessToken,
		RefreshToken: rec.RefreshToken,
		TokenType:    rec.TokenType,
		Expiry:       rec.Expiry,
	}
	cfg := &oauth2.Config{
		ClientID: rec.ClientID,
		// The client is public and registered with token_endpoint_auth_method "none",
		// so the client_id goes in the body and no basic-auth header may be sent.
		Endpoint: oauth2.Endpoint{TokenURL: rec.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
	// Not a request context: oauth2 keeps this one for every later refresh, so a
	// per-request context would break refreshing once that request finished.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, s.client)
	return s.persisting(server, rec, cfg.TokenSource(ctx, tok), tok)
}

func (s *Store) persisting(server string, meta record, src oauth2.TokenSource, held *oauth2.Token) *persistingSource {
	p := &persistingSource{store: s, server: server, meta: meta, src: src}
	if held != nil {
		p.lastAccess, p.lastRefresh = held.AccessToken, held.RefreshToken
	}
	return p
}

// persistingSource writes a token back whenever the underlying source hands out a
// new one. oauth2 refreshes lazily inside Token, so this is the whole write-back
// mechanism, and it is also the one place a permanently failed refresh is
// observable.
type persistingSource struct {
	store  *Store
	server string
	meta   record
	src    oauth2.TokenSource

	mu          sync.Mutex
	lastAccess  string
	lastRefresh string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		// Only a refusal the grant cannot recover from. The SDK swallows an invalid_grant
		// and sends the request unauthenticated, so the 401 that follows would otherwise
		// start a browser authorization the user never asked for; a transient fault must
		// not do that and must not ask the user to act. The token itself is never in the
		// note: it is rendered on the status page.
		if p.permanent(err) {
			p.store.rejectGrant(p.server)
			p.store.needsAuth(p.server, needsUserNote)
		}
		return nil, err
	}
	// The provider honoured the grant, so nothing stands against it any more.
	p.store.grantWorks(p.server)

	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken == p.lastAccess && tok.RefreshToken == p.lastRefresh {
		return tok, nil
	}
	if err := p.persistLocked(tok); err != nil {
		// Not fatal to the request: the token in hand still works, and losing it
		// costs a re-authorization after the next restart rather than an outage now.
		slog.Error("persist oauth token", "backend", p.server, "error", err)
	}
	return tok, nil
}

// permanent reports whether err means the stored grant can never work again, which is
// the only case where the user has to act. A network fault, a provider 5xx or a
// temporarily_unavailable is a blip: latching on one would turn it into a permanent
// demand for a click.
func (p *persistingSource) permanent(err error) bool {
	var refused *oauth2.RetrieveError
	if errors.As(err, &refused) {
		// RFC 6749 section 5.2: these two mean the grant or the client is gone.
		return refused.ErrorCode == "invalid_grant" || refused.ErrorCode == "invalid_client"
	}
	// No refresh token was ever held, so oauth2 refused before any request: the access
	// token has expired and only a new authorization can replace it.
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastRefresh == ""
}

func (p *persistingSource) persist(tok *oauth2.Token) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistLocked(tok)
}

func (p *persistingSource) persistLocked(tok *oauth2.Token) error {
	rec := p.meta
	rec.AccessToken = tok.AccessToken
	rec.RefreshToken = tok.RefreshToken
	rec.TokenType = tok.Type()
	rec.Expiry = tok.Expiry
	if err := p.store.save(p.server, rec); err != nil {
		return err
	}
	p.lastAccess, p.lastRefresh = tok.AccessToken, tok.RefreshToken
	return nil
}

func (s *Store) needsAuth(server, note string) {
	if s.hooks.NeedsAuth != nil {
		s.hooks.NeedsAuth(server, note)
	}
}

func (s *Store) authorized(server string) {
	if s.hooks.Authorized != nil {
		s.hooks.Authorized(server)
	}
}

// path is the token file for server. The name is a key from the user's config, so
// it is escaped rather than trusted to be a single path element.
func (s *Store) path(server string) string {
	return filepath.Join(s.dir, "oauth-"+url.PathEscape(server)+".json")
}

func (s *Store) load(server string) (*record, error) {
	raw, err := os.ReadFile(s.path(server))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read oauth state for %s: %w", server, err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("parse oauth state for %s: %w", server, err)
	}
	return &rec, nil
}

// save replaces server's token file atomically: a reader must never see a
// half-written credential, and a crash must not leave one behind. No error here
// carries the record, which holds the token.
func (s *Store) save(server string, rec record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal oauth state for %s: %w", server, err)
	}
	if err := os.MkdirAll(s.dir, stateDirMode); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(s.dir, stateDirMode); err != nil {
		return fmt.Errorf("restrict state directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".oauth-*")
	if err != nil {
		return fmt.Errorf("create temporary oauth state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(tokenFileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict oauth state: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write oauth state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write oauth state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path(server)); err != nil {
		return fmt.Errorf("replace oauth state: %w", err)
	}
	return nil
}

// handler wraps the SDK's authorization-code handler with mcpd's two deviations
// from it: a 403 never starts an authorization flow, and neither does a grant the
// provider has already rejected.
type handler struct {
	store  *Store
	server string

	mu sync.Mutex
	// inner is rebuilt when registeredAs falls behind the persisted client_id.
	inner        *auth.AuthorizationCodeHandler
	registeredAs string
	issuer       string
}

func (h *handler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	inner := h.inner
	h.mu.Unlock()
	return inner.TokenSource(ctx)
}

// Authorize runs the flow for a 401, and refuses to run it for a 403 or for a
// grant the provider has already rejected.
//
// The 403 refusal is because the SDK retries the identical request after a
// successful Authorize, and mcpd's at-most-once tool dispatch is enforced above the
// transport, so that retry is outside its boundary: a 401 is refused before the
// resource server acts on the request, but a 403 can follow partial processing,
// where a replay would repeat a side effect. Returning an error is what suppresses
// the retry; the cost is step-up authorization for an insufficient_scope 403.
//
// The rejected-grant refusal is what keeps a dead refresh token from starting a
// fresh browser authorization on every refresh attempt, each parking a connect for
// its whole timeout. Only the user's authenticate action lifts it.
func (h *handler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden {
		// The interface makes the callee responsible for the body.
		resp.Body.Close()
		return fmt.Errorf("backend %s answered 403: mcpd does not authorize on a 403, because the transport would then replay the request", h.server)
	}
	if !h.store.permitAuthorization(h.server) {
		resp.Body.Close()
		h.store.needsAuth(h.server, needsUserNote)
		return fmt.Errorf("backend %s needs authorizing again: its stored grant was rejected, so use the authenticate action", h.server)
	}
	h.noteIssuer(ctx, req, resp)

	inner, err := h.authorizer()
	if err != nil {
		resp.Body.Close()
		return err
	}
	if err := inner.Authorize(ctx, req, resp); err != nil {
		return err
	}
	// An authorization can succeed on a session that never went down, and then no
	// handshake runs to clear the needs-auth this flow published.
	h.store.authorized(h.server)
	return nil
}

// authorizer returns the SDK handler to authorize with, rebuilding it when a
// client_id has been persisted that the current one was not built with. The SDK
// consumes its configuration at construction and documents that it must not be
// modified afterwards, so reusing a newly registered client_id means building a new
// handler. Without this, a second authorization in one daemon run registers a second
// client at the provider and orphans the first.
func (h *handler) authorizer() (*auth.AuthorizationCodeHandler, error) {
	rec, err := h.store.load(h.server)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec == nil || rec.ClientID == "" || rec.ClientID == h.registeredAs {
		return h.inner, nil
	}
	// The token is restored here whatever the grant state: this rebuild happens inside
	// an authorization that is about to install a fresh source anyway.
	inner, err := h.store.newAuthorizer(h, rec, true)
	if err != nil {
		return nil, err
	}
	h.inner, h.registeredAs = inner, rec.ClientID
	return inner, nil
}

// noteIssuer learns the authorization server's identity from the same challenge
// the SDK's own discovery starts from, because nothing the SDK hands back carries
// it and the persisted issuer is what binds a stored client_id to the server it
// was registered with. Best effort: the SDK repeats this discovery for itself, so
// a failure here costs only that binding.
func (h *handler) noteIssuer(ctx context.Context, req *http.Request, resp *http.Response) {
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return
	}
	metaURL := ""
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			metaURL = u
			break
		}
	}
	if metaURL == "" {
		return
	}
	prm, err := oauthex.GetProtectedResourceMetadata(ctx, metaURL, req.URL.String(), h.store.client)
	if err != nil || len(prm.AuthorizationServers) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.issuer = prm.AuthorizationServers[0]
}

func (h *handler) issuerSeen() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.issuer
}
