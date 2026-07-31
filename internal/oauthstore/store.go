// Package oauthstore owns mcpd's downstream OAuth: the token it holds per
// OAuth-gated backend, the authorizations awaiting a browser callback, and the
// handler a backend's HTTP transport authorizes with.
package oauthstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/ahodges/mcpd/internal/config"
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

	// Declared reports the current declaration identity for a backend, or false once it
	// is no longer declared. It is a hook rather than a direct dependency because this
	// store is consulted from beneath the backend locks, where taking the daemon's
	// outermost operation lock would invert the lock order.
	Declared func(server string) (config.Identity, bool)
	// Held, when set, runs fn while the declared set cannot change, reporting whether
	// server is declared under want. It is what makes a token write atomic against a
	// concurrent removal rather than merely preceded by a check.
	Held func(server string, want config.Identity, fn func()) bool

	mu       sync.Mutex
	handlers map[string]auth.OAuthHandler
	// builtFor records the identity each cached handler was built for. The comparison
	// has to happen here rather than only on the record read: a cache hit never reaches
	// disk, and the cached handler owns a live token source holding the token in memory.
	builtFor map[string]config.Identity
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
		builtFor:    make(map[string]config.Identity),
		pending:     make(map[string]*pending),
		grants:      make(map[string]grantState),
	}
}

// grantState is what the store knows about a backend's stored grant, and the two
// fields answer two different questions: unusable decides whether an automatic dial
// may present the grant or authorize without it, and requested decides whether the
// user has bought an authorization the automatic answer would have refused.
type grantState struct {
	unusable  bool
	requested bool
}

// AllowAuthorization records that the user has asked server to be authorized, which
// is what lets an authorization start once the stored grant is known to be unusable.
// Without a request, that grant waits for a person: re-entering the browser flow on
// the daemon's own refresh cadence publishes an authorization nobody asked for and
// parks a connect on every attempt.
func (s *Store) AllowAuthorization(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	g.requested = true
	s.grants[server] = g
	if g.unusable {
		s.forgetLocked(server)
	}
}

// DiscardStoredGrant records that the stored grant does not work, on the evidence of a
// reconnect the user asked for that produced neither a token nor an authorization. It
// discards the grant from use, never from disk: a completed authorization overwrites
// the file and nothing else removes it.
//
// This is the explicit half of the same question rejectGrant answers automatically,
// and it deliberately asks nothing about why the provider refused. The classifier
// behind rejectGrant exists to decide whether a dial may authorize by itself, which is
// a judgement about one error code; recovery must not inherit that judgement, or a
// refusal the classifier does not recognise could never be recovered from at all.
func (s *Store) DiscardStoredGrant(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	g.unusable = true
	s.grants[server] = g
	s.forgetLocked(server)
}

// rejectGrant records that the provider refused this backend's grant in a way it
// cannot recover from. It leaves a request the user has already made intact, so a
// refresh failure on the session being torn down cannot cancel the authorization they
// asked for.
func (s *Store) rejectGrant(server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[server]
	g.unusable = true
	s.grants[server] = g
}

// forgetLocked drops the cached handler so the next dial builds one from the current
// state. It is what stops an unusable grant being presented: a request that cannot
// obtain a token is never sent, so it never reaches the 401 an authorization starts
// from.
func (s *Store) forgetLocked(server string) {
	delete(s.handlers, server)
}

// grantWorks records that the provider has honoured the grant, which is what keeps a
// refusal from outliving the grant it was about. Without it one network blip would
// block automatic re-authorization for the life of the process.
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
	return !g.unusable
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
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return res, err
	}
	reportProviderRefusal(req, res)
	return res, nil
}

// providerErrorLimit caps how much of a refusal is read back. An OAuth error response is a
// small JSON object; anything larger is not one.
const providerErrorLimit = 4 << 10

// reportProviderRefusal logs why a provider refused an OAuth request.
//
// Without it a failed code exchange leaves no trace of its cause, because the SDK's response
// to a refused exchange is to begin a fresh authorization: the reason sits in a response body
// nothing else looks at, and the error the user is finally shown belongs to that second
// attempt rather than to the failure that caused it.
//
// Only a refusal is read, and only its RFC 6749 error fields are logged. A successful response
// carries the token and is never touched, and the request body, which carries the
// authorization code and the PKCE verifier, is never read here at all.
func reportProviderRefusal(req *http.Request, res *http.Response) {
	if res.StatusCode < 400 || res.Body == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, providerErrorLimit))
	res.Body.Close()
	// Put it back: this is a diagnostic, and the SDK still has to read the response it
	// would have read had nothing been logged.
	res.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return
	}
	var refusal struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
		URI         string `json:"error_uri"`
	}
	// A body that is not an OAuth error object is reported by its status alone rather than
	// echoed: an HTML error page from a proxy in front of the provider does not belong in a
	// log line.
	_ = json.Unmarshal(body, &refusal)
	slog.Warn("the authorization provider refused a request",
		"endpoint", req.URL.Path, "status", res.StatusCode,
		"error", refusal.Error, "description", refusal.Description, "uri", refusal.URI)
}

// record is one backend's persisted OAuth state. The client_id is kept because
// re-registering on every start would churn clients at the provider, and the
// issuer and token endpoint are kept so a restart can restore a token source
// without a discovery round trip.
type record struct {
	// Identity is the declaration this grant was issued under. A record is keyed only
	// by backend name, and the name says nothing about which provider issued the token
	// or for which resource, so without this a repointed backend would present a token
	// to an endpoint it was never issued for.
	Identity     config.Identity `json:"identity"`
	ClientID     string          `json:"client_id"`
	Issuer       string          `json:"issuer,omitempty"`
	TokenURL     string          `json:"token_url"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	TokenType    string          `json:"token_type,omitempty"`
	Expiry       time.Time       `json:"expiry"`
}

// Handler returns the authorization handler for server, built on first use from
// whatever has been persisted. The same wrapper is returned every time, so the
// transport of a reconnected session keeps a stable reference; the SDK handler
// inside it is rebuilt when a newly registered client_id has to be picked up.
func (s *Store) Handler(server string) (auth.OAuthHandler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The identity check comes first and applies to the cached handler too. A cache hit
	// never reaches disk, so a check placed on the record read alone would never run for
	// a primed handler, and that handler holds a usable token in memory.
	want, declared := config.Identity{}, true
	if s.Declared != nil {
		want, declared = s.Declared(server)
		if !declared {
			return nil, fmt.Errorf("%s: %w", server, ErrUndeclared)
		}
	}
	if h, ok := s.handlers[server]; ok {
		if s.Declared == nil || s.builtFor[server] == want {
			return h, nil
		}
		s.discardLocked(server)
	}
	rec, err := s.load(server)
	if err != nil {
		return nil, err
	}
	// A stored grant issued under a different declaration is unusable rather than
	// merely stale: it is discarded, deleted, and the backend is reported as needing
	// authorization. An absent identity counts as a mismatch, because a record written
	// before identities existed cannot be shown to belong here.
	if rec != nil && s.Declared != nil && rec.Identity != want {
		s.deleteLocked(server)
		rec = nil
		s.hooks.NeedsAuth(server, repointedNote)
	}
	h := &handler{store: s, server: server, identity: want}
	if rec != nil {
		h.registeredAs = rec.ClientID
	}
	// A grant the user has just asked to replace is not presented: doing so is what
	// keeps a dead token source from failing every request before one can 401.
	// An unusable grant is not presented: a token source that cannot produce a token
	// fails every request before one can 401, and the 401 is what an authorization
	// starts from.
	inner, err := s.newAuthorizer(h, rec, !s.grants[server].unusable)
	if err != nil {
		return nil, err
	}
	h.inner = inner
	s.handlers[server] = h
	s.builtFor[server] = want
	return h, nil
}

// ErrUndeclared reports that a backend is no longer declared, so nothing may be
// authenticated or persisted for it.
var ErrUndeclared = errors.New("backend no longer declared")

const repointedNote = "declaration changed since authorization; re-authorize"

// Forget discards everything held for a backend, in memory and on disk. Deleting the
// record alone would remove only the copy that was not being used: the cached handler
// owns the live token source.
func (s *Store) Forget(server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discardLocked(server)
	return s.deleteLocked(server)
}

func (s *Store) discardLocked(server string) {
	delete(s.handlers, server)
	delete(s.builtFor, server)
	delete(s.grants, server)
	for state, p := range s.pending {
		if p.server == server {
			delete(s.pending, state)
		}
	}
}

func (s *Store) deleteLocked(server string) error {
	if err := os.Remove(s.path(server)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete oauth state for %s: %w", server, err)
	}
	return nil
}

// Reconcile settles stored grants against the current declarations: a record for a
// backend that is not declared, or one issued under a different declaration, is
// discarded and deleted.
func (s *Store) Reconcile(declared map[string]config.Identity) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state directory: %w", err)
	}
	for _, e := range entries {
		server, ok := serverFromFile(e.Name())
		if !ok {
			continue
		}
		s.mu.Lock()
		rec, loadErr := s.load(server)
		want, isDeclared := declared[server]
		if loadErr == nil && (!isDeclared || rec == nil || rec.Identity != want) {
			s.discardLocked(server)
			if err := s.deleteLocked(server); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		s.mu.Unlock()
	}
	return nil
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
				Identity: h.identity,
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
	// A refresh must not resurrect a record for a backend that has been removed, nor
	// write one under a declaration it was not issued for. The check and the write are
	// one critical section against the declared set, so a write that saw the backend as
	// declared has landed before that removal's cleanup runs, and a later one is refused.
	// Refusing loses nothing: the token in hand still serves this request.
	if p.store.Declared != nil {
		saved := false
		var err error
		if !p.store.holdDeclared(p.server, rec.Identity, func() {
			err = p.store.save(p.server, rec)
			saved = true
		}) {
			return fmt.Errorf("%s: %w", p.server, ErrUndeclared)
		}
		if err != nil {
			return err
		}
		if !saved {
			return fmt.Errorf("%s: %w", p.server, ErrUndeclared)
		}
	} else if err := p.store.save(p.server, rec); err != nil {
		return err
	}
	p.lastAccess, p.lastRefresh = tok.AccessToken, tok.RefreshToken
	return nil
}

// holdDeclared runs fn only while server is declared under want, and holds that answer
// for the duration of fn.
func (s *Store) holdDeclared(server string, want config.Identity, fn func()) bool {
	if s.Held != nil {
		return s.Held(server, want, fn)
	}
	id, ok := s.Declared(server)
	if !ok || id != want {
		return false
	}
	fn()
	return true
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

// serverFromFile is path's inverse, used by Reconcile to find records whose backend is
// gone from the declarations entirely.
func serverFromFile(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "oauth-")
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, ".json")
	if !ok {
		return "", false
	}
	server, err := url.PathUnescape(rest)
	if err != nil {
		return "", false
	}
	return server, true
}

// TokenExpiry reports when server's stored access token expires, which the status
// surface lists. It is the only field of the record that ever leaves this package:
// the rest of it is the credential. A backend with no stored token, or one whose
// record cannot be read, reports nothing rather than a zero time that renders as a
// real reading.
func (s *Store) TokenExpiry(server string) (time.Time, bool) {
	rec, err := s.load(server)
	if err != nil || rec == nil || rec.Expiry.IsZero() {
		return time.Time{}, false
	}
	return rec.Expiry, true
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
	// identity is the declaration this handler was built for, carried so a persisted
	// refresh records the same one.
	identity config.Identity

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
