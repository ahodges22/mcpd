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
)

// Hooks are the backend transitions the store drives.
type Hooks struct {
	// NeedsAuth moves a backend to needs-auth, with note rendered as its auth note.
	// It is called while an authorization waits on the user, and when a token
	// refresh has failed so the user must authorize again.
	NeedsAuth func(server, note string)
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
	// added is closed and replaced whenever a pending authorization is published,
	// so Await can block for one without polling.
	added chan struct{}
}

// New returns a store persisting under dir and bridging the authorization flow
// through redirectURL, which must be the daemon's own callback route: it is
// registered with the provider as the redirect URI and must not move.
func New(dir, redirectURL string, hooks Hooks) *Store {
	return &Store{
		dir:         dir,
		redirectURL: redirectURL,
		hooks:       hooks,
		client:      &http.Client{Timeout: providerTimeout},
		handlers:    make(map[string]auth.OAuthHandler),
		pending:     make(map[string]*pending),
		added:       make(chan struct{}),
	}
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
// whatever has been persisted. The same handler is returned every time: it owns
// the live token source, so a reconnect must not construct one and re-authorize.
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
		AuthorizationCodeFetcher: s.fetchCode(server),
		RequestRefreshToken:      true,
		Client:                   s.client,
		NewTokenSource: func(ctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			src := s.persisting(server, record{
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
			cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: rec.ClientID, Issuer: rec.Issuer}
		}
		cfg.InitialTokenSource = s.restore(server, *rec)
	}
	inner, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return nil, fmt.Errorf("oauth handler for %s: %w", server, err)
	}
	h.inner = inner
	s.handlers[server] = h
	return h, nil
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
		// The token itself is never in this note: it is rendered on the status page.
		p.store.needsAuth(p.server, "token refresh failed, authorize again")
		return nil, err
	}
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

// handler wraps the SDK's authorization-code handler with mcpd's one deviation
// from it: a 403 never starts an authorization flow.
type handler struct {
	inner  *auth.AuthorizationCodeHandler
	store  *Store
	server string

	mu     sync.Mutex
	issuer string
}

func (h *handler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.inner.TokenSource(ctx)
}

// Authorize runs the flow for a 401 and refuses to run it for a 403. The SDK
// retries the identical request after a successful Authorize, and mcpd's
// at-most-once tool dispatch is enforced above the transport, so that retry is
// outside its boundary: a 401 is refused before the resource server acts on the
// request, but a 403 can follow partial processing, where a replay would repeat a
// side effect. Returning an error is what suppresses the retry; the cost is
// step-up authorization for an insufficient_scope 403.
func (h *handler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden {
		// The interface makes the callee responsible for the body.
		resp.Body.Close()
		return fmt.Errorf("backend %s answered 403: mcpd does not authorize on a 403, because the transport would then replay the request", h.server)
	}
	h.noteIssuer(ctx, req, resp)
	return h.inner.Authorize(ctx, req, resp)
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
