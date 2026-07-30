package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/oauthstore"
)

const (
	// transitionTimeout bounds the wait on an enable, disable or reconnect. Those
	// drain the dispatch gate and close a session and cannot be cancelled, so this
	// bounds the response rather than the work; 10s covers an ordinary teardown,
	// whose stdio escalation only begins once the session is idle.
	transitionTimeout = 10 * time.Second
	// reindexTimeout bounds the wait on a re-index, which waits on every backend's
	// list and so on every backend that has to dial first. 20s answers inside a
	// browser's patience while the refresh runs on to completion behind it.
	reindexTimeout = 20 * time.Second
	// invokeCallBudget is the inspector's tool-call budget, added to the backend's
	// own handshake budget rather than replacing it, exactly as the catalog's read
	// deadline is, so a cold start the configuration permits is never truncated.
	invokeCallBudget = 60 * time.Second
	// authorizePollInterval is how often that wait rechecks. A browser is waiting on
	// it, so it is short enough to be imperceptible and each check is two map reads.
	authorizePollInterval = 50 * time.Millisecond
)

// AuthorizeWaitTimeout bounds one authorization attempt's wait for an authorization URL
// to appear. The URL exists only once the backend's 401 has been discovered, which is up
// to four round trips to the provider including a client registration. It is a variable
// so a test can stage a refusal slower than the budget without spending the budget in
// real time.
var AuthorizeWaitTimeout = 30 * time.Second

//go:embed assets
var assetFS embed.FS

// Server is the loopback web surface. It is constructed without a listener, so it
// is testable on its own; cmd/mcpd mounts Handler on its own mux.
type Server struct {
	reg   *backend.Registry
	cat   *catalog.Catalog
	guard *Guard
	oauth *oauthstore.Store
}

// route is one registered path. Patterns carry no method, because the method check
// belongs in one reviewable place rather than being implied by the mux's routing.
type route struct {
	method  string
	path    string
	mutates bool // changes daemon or backend state, so it is POST plus JSON only
	// nonceGuarded marks the single documented exemption from that rule: the OAuth
	// callback is necessarily a top-level browser GET, so a one-time state nonce
	// authorizes it instead of the method.
	nonceGuarded bool
	handler      http.HandlerFunc
}

func New(reg *backend.Registry, cat *catalog.Catalog, g *Guard, oauth *oauthstore.Store) *Server {
	return &Server{reg: reg, cat: cat, guard: g, oauth: oauth}
}

// Handler is the web surface behind the shared cross-origin guard and the
// DNS-rebinding check, with the Host check outermost because a rebound request is
// misdirected before it is anything else.
//
// The mux is built here and kept local rather than held as a field: with no mux to
// reach, routes cannot be registered anywhere but routes(), which is what makes the
// GET-safety tests' enumeration of routes() sound rather than merely current.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.path, guardMethod(s.guard, rt, rt.handler))
	}
	return s.guard.RequireLoopbackHost(s.guard.Protect(mux))
}

func (s *Server) routes() []route {
	return []route{
		// Exact match rather than a subtree: a catch-all root answers every unknown path
		// with the status page, which hides anything registered outside this table.
		{method: http.MethodGet, path: "/{$}", handler: s.statusPage},
		{method: http.MethodGet, path: "/api/status", handler: s.statusAPI},
		{method: http.MethodGet, path: "/inspect/{name}", handler: s.inspectPage},
		{method: http.MethodGet, path: "/assets/", handler: http.FileServerFS(assetFS).ServeHTTP},
		{method: http.MethodGet, path: "/oauth/callback", mutates: true, nonceGuarded: true,
			handler: s.oauthCallback},
		{method: http.MethodPost, path: "/api/backends/{name}/authorize", mutates: true,
			handler: s.authorize},
		{method: http.MethodPost, path: "/api/backends/{name}/enable", mutates: true,
			handler: s.backendAction(s.reg.Enable)},
		{method: http.MethodPost, path: "/api/backends/{name}/disable", mutates: true,
			handler: s.backendAction(s.reg.Disable)},
		{method: http.MethodPost, path: "/api/backends/{name}/reconnect", mutates: true,
			handler: s.backendAction(s.reg.Reconnect)},
		{method: http.MethodPost, path: "/api/reconnect-all", mutates: true, handler: s.reconnectAll},
		{method: http.MethodPost, path: "/api/reindex", mutates: true, handler: s.reindex},
		{method: http.MethodPost, path: "/api/invoke", mutates: true, handler: s.invoke},
	}
}

func (s *Server) statusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *Server) backendAction(op func(string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := s.reg.Get(name); !ok {
			http.Error(w, "unknown backend", http.StatusNotFound)
			return
		}
		runTransition(w, transitionTimeout, func() error { return op(name) })
	}
}

func (s *Server) reconnectAll(w http.ResponseWriter, _ *http.Request) {
	runTransition(w, transitionTimeout, func() error {
		var errs []error
		for _, name := range s.reg.Names() {
			// A disabled backend is skipped, not an error: reconnect-all must not
			// resurrect what the kill switch killed, and there is nothing to report.
			if err := s.reg.Reconnect(name); err != nil && !errors.Is(err, backend.ErrDisabled) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

func (s *Server) reindex(w http.ResponseWriter, _ *http.Request) {
	runTransition(w, reindexTimeout, func() error {
		// Not the request's context: RefreshAll's context bounds the wait rather than
		// the work, and an abandoned request must not cancel a refresh other triggers
		// are waiting on.
		s.cat.RefreshAll(context.Background())
		return nil
	})
}

type invokeRequest struct {
	ID        string         `json:"id"`
	Arguments map[string]any `json:"arguments"`
}

type invokeResponse struct {
	Result *mcp.CallToolResult `json:"result,omitempty"`
	Error  string              `json:"error,omitempty"`
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	var in invokeRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, invokeResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	entry, ok := s.cat.Lookup(in.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, invokeResponse{Error: "unknown tool id " + in.ID})
		return
	}
	b, ok := s.reg.Get(entry.Server)
	if !ok {
		writeJSON(w, http.StatusNotFound, invokeResponse{Error: "unknown backend " + entry.Server})
		return
	}
	// The backend's handshake budget is added rather than shared, so an invoke that
	// has to dial first is not cut short by a deadline the configuration never saw.
	ctx, cancel := context.WithTimeout(r.Context(), b.ConnectTimeout()+invokeCallBudget)
	defer cancel()

	res, err := b.Call(ctx, entry.Tool, in.Arguments)
	if err != nil {
		// Forwarded verbatim: it is the only signal that tells the user whether the
		// send was never attempted or whether the outcome is unknown.
		writeJSON(w, http.StatusBadGateway, invokeResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, invokeResponse{Result: res})
}

// oauthCallback is the one route that changes state on a GET: the provider
// redirects the browser here, so the method rule cannot apply and the one-time
// state nonce authorizes it instead. Nothing from the query reaches the response:
// every parameter here is attacker reachable.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := s.oauth.Deliver(q.Get("state"), q.Get("code"), q.Get("iss")); err != nil {
		http.Error(w, "this callback matches no outstanding authorization request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write([]byte("Authorization received. You can close this tab and return to the mcpd status page.\n"))
}

// authorize starts an authorization the user can complete and answers with the URL
// to send them to.
//
// Nothing is torn down before it is established that an authorization is needed: a
// backend that is up is answered from its health record, and one already waiting on
// the browser is answered with the URL it is waiting on. Only then does it
// reconnect, which is what lets a backend sitting in its failure backoff reach the
// 401 an authorization URL is discovered from.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b, ok := s.reg.Get(name)
	if !ok {
		http.Error(w, "unknown backend", http.StatusNotFound)
		return
	}
	if !b.UsesOAuth() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this backend does not authorize with oauth"})
		return
	}
	if authURL, pending := s.oauth.Pending(name); pending {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending", "authorize_url": authURL})
		return
	}
	if b.Health().State == backend.StateUp {
		writeJSON(w, http.StatusOK, map[string]string{"status": "authorized"})
		return
	}
	// The user asking is what lifts the refusal an unusable grant leaves behind.
	s.oauth.AllowAuthorization(name)

	// Only a failed attempt from this reconnect's generation permits discarding the grant.
	for attempt := range 2 {
		if attempt > 0 {
			s.oauth.DiscardStoredGrant(name)
		}
		var generation uint64
		finished, err := awaitTransition(transitionTimeout, func() error {
			var err error
			generation, err = s.reg.ReconnectGeneration(name)
			return err
		})
		if !finished {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "in-progress",
				"error":  "the reconnect cannot be cancelled and is still running; it will finish in the background",
			})
			return
		}
		if err != nil {
			writeJSON(w, transitionStatus(err), map[string]string{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), AuthorizeWaitTimeout)
		authURL, outcome := s.awaitAuthorization(ctx, b, name, generation)
		cancel()
		switch outcome {
		case authorizePending:
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending", "authorize_url": authURL})
			return
		case authorizeServing:
			writeJSON(w, http.StatusOK, map[string]string{"status": "authorized"})
			return
		case authorizeSuperseded:
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "another lifecycle transition superseded this authorization attempt",
			})
			return
		}
	}
	writeJSON(w, http.StatusGatewayTimeout, map[string]string{
		"error": "no authorization was started; the backend's own error is on the status page",
	})
}

// authorizeOutcome is what one reconnect produced.
type authorizeOutcome int

const (
	// authorizeUnusable: neither of the other two, so the stored grant is not working.
	authorizeUnusable authorizeOutcome = iota
	authorizePending
	authorizeServing
	authorizeSuperseded
)

// awaitAuthorization waits for one of the four outcomes of a reconnect, counting a
// failed handshake as evidence against the stored grant only when that handshake began
// in the generation this reconnect produced.
func (s *Server) awaitAuthorization(ctx context.Context, b *backend.Backend, name string, generation uint64) (string, authorizeOutcome) {
	tick := time.NewTicker(authorizePollInterval)
	defer tick.Stop()
	for {
		if authURL, pending := s.oauth.Pending(name); pending {
			return authURL, authorizePending
		}
		attempt, currentGeneration := b.ConnectAttempt()
		switch h := b.Health(); {
		case h.State == backend.StateUp:
			return "", authorizeServing
		case currentGeneration != generation:
			return "", authorizeSuperseded
		case attempt.Generation == generation && attempt.Failed:
			return "", authorizeUnusable
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return "", authorizeUnusable
		}
	}
}

// runTransition waits a bounded time for an operation that cannot be cancelled.
// Registry.Enable, Disable and Reconnect take no context and can take none, because
// gate.Lock has no cancellable variant. So on expiry the response gives up while
// the work continues to completion, rather than the work being abandoned.
func runTransition(w http.ResponseWriter, timeout time.Duration, op func() error) {
	finished, err := awaitTransition(timeout, op)
	switch {
	case !finished:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "in-progress",
			"error":  "the transition cannot be cancelled and is still running; it will finish in the background",
		})
	case err != nil:
		writeJSON(w, transitionStatus(err), map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// awaitTransition runs op and reports whether it finished within timeout. The work
// continues either way, because it cannot be cancelled.
func awaitTransition(timeout time.Duration, op func() error) (finished bool, err error) {
	done := make(chan error, 1)
	go func() { done <- op() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return true, err
	case <-timer.C:
		return false, nil
	}
}

func transitionStatus(err error) int {
	if errors.Is(err, backend.ErrDisabled) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Backend-derived text reaches the browser in this body, so it must never be
	// sniffed as anything but JSON.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(body)
}
