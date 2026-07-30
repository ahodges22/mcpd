package oauthstore_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/testfake"
)

// provider is a fake OAuth-protected MCP server. It imitates what real Notion did,
// as recorded in PHASE0.md: an unauthenticated request answers 401 with a
// WWW-Authenticate challenge naming the protected-resource metadata under the MCP
// endpoint's own path, the client registers dynamically as a public client with no
// secret, and the token endpoint requires PKCE S256.
type provider struct {
	ts  *httptest.Server
	mcp http.Handler

	// forbid answers the MCP endpoint 403 rather than 401, so the ruling that a 403
	// never authorizes is testable.
	forbid atomic.Bool
	// refusal, when set, is the OAuth error code the refresh grant answers with, so
	// both classes of permanent refusal are testable: the SDK aborts a request on most
	// of them, but swallows invalid_grant and sends the request unauthenticated.
	refusal atomic.Pointer[string]

	registrations  atomic.Int64
	authorizations atomic.Int64
	exchanges      atomic.Int64
	refreshes      atomic.Int64
	challenges     atomic.Int64
	resourceMeta   atomic.Int64
	bearerServed   atomic.Int64

	mu       sync.Mutex
	methods  []string // JSON-RPC methods the MCP endpoint received, in order
	clients  map[string]string
	codes    map[string]grant
	access   map[string]bool
	refresh  map[string]bool
	issued   int
	assigned int
}

// grant is one issued authorization code and what it is bound to.
type grant struct {
	clientID  string
	challenge string
	redirect  string
}

type counts struct {
	registrations, authorizations, exchanges, refreshes, challenges, resourceMeta int64
}

func newProvider(t *testing.T, fake *testfake.Fake) *provider {
	t.Helper()
	p := &provider{
		clients: make(map[string]string),
		codes:   make(map[string]grant),
		access:  make(map[string]bool),
		refresh: make(map[string]bool),
	}
	p.mcp = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return fake.Server() },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", p.serveMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", p.serveResourceMeta)
	mux.HandleFunc("/.well-known/oauth-authorization-server", p.serveAuthServerMeta)
	mux.HandleFunc("/register", p.serveRegister)
	mux.HandleFunc("/authorize", p.serveAuthorize)
	mux.HandleFunc("/token", p.serveToken)
	p.ts = httptest.NewServer(mux)
	t.Cleanup(func() {
		p.ts.CloseClientConnections()
		p.ts.Close()
		fake.Close()
	})
	return p
}

func (p *provider) mcpURL() string { return p.ts.URL + "/mcp" }

func (p *provider) counts() counts {
	return counts{
		registrations:  p.registrations.Load(),
		authorizations: p.authorizations.Load(),
		exchanges:      p.exchanges.Load(),
		refreshes:      p.refreshes.Load(),
		challenges:     p.challenges.Load(),
		resourceMeta:   p.resourceMeta.Load(),
	}
}

// requests reports how many JSON-RPC requests the MCP endpoint has served, which is
// how a session torn down and re-established shows up.
func (p *provider) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.methods)
}

// repeated reports any JSON-RPC method the MCP endpoint received more than once,
// which is how a retried request shows up.
func (p *provider) repeated() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]int, len(p.methods))
	var out []string
	for _, m := range p.methods {
		seen[m]++
		if seen[m] == 2 {
			out = append(out, m)
		}
	}
	return out
}

func (p *provider) serveMCP(w http.ResponseWriter, r *http.Request) {
	p.recordMethod(r)
	if p.forbid.Load() {
		p.challenge(w, http.StatusForbidden)
		return
	}
	if !p.validAccess(bearer(r)) {
		p.challenge(w, http.StatusUnauthorized)
		return
	}
	p.bearerServed.Add(1)
	p.mcp.ServeHTTP(w, r)
}

// challenge answers the way Notion did: the metadata pointer is in the challenge,
// carrying the endpoint's own path suffix, because a client that guessed a
// well-known path would have to be right about a detail the challenge simply tells
// it.
func (p *provider) challenge(w http.ResponseWriter, status int) {
	p.challenges.Add(1)
	w.Header().Set("WWW-Authenticate",
		`Bearer realm="OAuth", resource_metadata="`+p.ts.URL+`/.well-known/oauth-protected-resource/mcp"`)
	http.Error(w, "unauthorized", status)
}

func (p *provider) serveResourceMeta(w http.ResponseWriter, _ *http.Request) {
	p.resourceMeta.Add(1)
	// resource must equal the URL the client requested, because the SDK checks that
	// the two agree.
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              p.mcpURL(),
		"authorization_servers": []string{p.ts.URL},
	})
}

func (p *provider) serveAuthServerMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.ts.URL,
		"authorization_endpoint":                p.ts.URL + "/authorize",
		"token_endpoint":                        p.ts.URL + "/token",
		"registration_endpoint":                 p.ts.URL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

func (p *provider) serveRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	p.registrations.Add(1)

	p.mu.Lock()
	p.assigned++
	id := fmt.Sprintf("client-%d", p.assigned)
	p.clients[id] = req.RedirectURIs[0]
	p.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": id,
		// Echoed verbatim, which is what the real provider did with a plain-HTTP
		// loopback redirect URI.
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                req.GrantTypes,
		"token_endpoint_auth_method": "none",
	})
}

// serveAuthorize is the endpoint the user's browser visits. It records the PKCE
// challenge and redirects back to the client's registered redirect URI.
func (p *provider) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	p.authorizations.Add(1)
	q := r.URL.Query()

	p.mu.Lock()
	redirect, known := p.clients[q.Get("client_id")]
	p.mu.Unlock()
	switch {
	case !known:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case q.Get("redirect_uri") != redirect:
		http.Error(w, "redirect_uri does not match the registration", http.StatusBadRequest)
		return
	case q.Get("response_type") != "code":
		http.Error(w, "unsupported response_type", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		http.Error(w, "S256 code_challenge is required", http.StatusBadRequest)
		return
	case q.Get("state") == "":
		http.Error(w, "state is required", http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	p.assigned++
	code := fmt.Sprintf("code-%d", p.assigned)
	p.codes[code] = grant{clientID: q.Get("client_id"), challenge: q.Get("code_challenge"), redirect: redirect}
	p.mu.Unlock()

	back, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "unparseable redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *provider) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		p.exchangeCode(w, r)
	case "refresh_token":
		p.refreshToken(w, r)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (p *provider) exchangeCode(w http.ResponseWriter, r *http.Request) {
	p.exchanges.Add(1)

	p.mu.Lock()
	code := r.Form.Get("code")
	g, ok := p.codes[code]
	delete(p.codes, code) // single use
	p.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	// PKCE is the only thing standing between a public client and a stolen code, so
	// the verifier is recomputed rather than assumed.
	if !pkceMatches(r.Form.Get("code_verifier"), g.challenge) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	if r.Form.Get("client_id") != g.clientID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client"})
		return
	}
	if r.Form.Get("redirect_uri") != g.redirect {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
		return
	}
	writeJSON(w, http.StatusOK, p.issue())
}

func (p *provider) refreshToken(w http.ResponseWriter, r *http.Request) {
	p.refreshes.Add(1)
	if code := p.refusal.Load(); code != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": *code, "error_description": "this grant is no longer usable"})
		return
	}
	p.mu.Lock()
	live := p.refresh[r.Form.Get("refresh_token")]
	p.mu.Unlock()
	if !live {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	writeJSON(w, http.StatusOK, p.issue())
}

func (p *provider) issue() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issued++
	access := fmt.Sprintf("access-%d", p.issued)
	refresh := fmt.Sprintf("refresh-%d", p.issued)
	p.access[access] = true
	p.refresh[refresh] = true
	return map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": refresh,
	}
}

// refuseRefresh makes the refresh grant answer with code from now on.
func (p *provider) refuseRefresh(code string) { p.refusal.Store(&code) }

// allowRefresh ends a refusal, which is what a provider recovering from a transient
// fault looks like.
func (p *provider) allowRefresh() { p.refusal.Store(nil) }

// revokeAccessTokens invalidates every access token issued so far, which is what a
// consent withdrawn at the provider looks like to a client still holding one: the
// token is unexpired, so nothing refreshes, and the next request simply 401s.
func (p *provider) revokeAccessTokens() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.access = make(map[string]bool)
}

func (p *provider) validAccess(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return token != "" && p.access[token]
}

// recordMethod notes which JSON-RPC method arrived, restoring the body for the
// real handler, so a test can tell a first attempt from a replay.
func (p *provider) recordMethod(r *http.Request) {
	if r.Method != http.MethodPost || r.Body == nil {
		return
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	if err != nil {
		return
	}
	var msg struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.methods = append(p.methods, msg.Method)
}

func pkceMatches(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func bearer(r *http.Request) string {
	after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return after
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(raw)
}
