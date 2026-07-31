package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/config"
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
	// manager owns adding, removing and reloading declarations. It is optional: the
	// three routes it serves are absent without it, which is what keeps every existing
	// test constructing a Server with four arguments.
	manager Manager
	// unvectorized is optional: it is nil when no embedding gateway is configured, and
	// the surface then reports nothing rather than a misleading zero.
	unvectorized func() int
}

// Manager is the part of the declaration-management layer this surface drives. Each
// call returns warnings that did not fail the operation alongside a fatal error.
type Manager interface {
	Add(name string, spec config.Backend) ([]error, error)
	Remove(name string) ([]error, error)
	Reload() ([]error, error)
}

// WithManager enables the add, remove and reload routes.
func (s *Server) WithManager(m Manager) *Server {
	s.manager = m
	return s
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

// New builds the web surface. All four arguments are required: the routes dereference
// every one of them, so a nil is a wiring mistake that fails at the first request
// rather than a case to answer for.
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
	base := []route{
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
	if s.manager == nil {
		return base
	}
	// Declaring a stdio backend starts a process, so these are the highest-privilege
	// operations on this surface. They are ordinary mutating routes, which is the point:
	// they inherit the loopback-host, origin and POST-plus-JSON guards unchanged.
	return append(base,
		route{method: http.MethodPost, path: "/api/backends", mutates: true, handler: s.addBackend},
		route{method: http.MethodPost, path: "/api/backends/{name}/remove", mutates: true, handler: s.removeBackend},
		route{method: http.MethodPost, path: "/api/reload", mutates: true, handler: s.reload},
	)
}

type addRequest struct {
	Name string         `json:"name"`
	Spec config.Backend `json:"spec"`
}

func (s *Server) addBackend(w http.ResponseWriter, r *http.Request) {
	var in addRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	runManaged(w, func() ([]error, error) { return s.manager.Add(in.Name, in.Spec) })
}

func (s *Server) removeBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	runManaged(w, func() ([]error, error) { return s.manager.Remove(name) })
}

func (s *Server) reload(w http.ResponseWriter, _ *http.Request) {
	runManaged(w, func() ([]error, error) { return s.manager.Reload() })
}

// runManaged answers a management operation. A warning is reported alongside success
// rather than as a failure: everything that can only warn happens after the declaration
// is already committed, so calling it a failure would tell the user nothing happened when
// something did.
func runManaged(w http.ResponseWriter, op func() ([]error, error)) {
	warnings, err := op()
	if err != nil {
		writeJSON(w, manageStatus(err), map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{"status": "ok"}
	if len(warnings) > 0 {
		notes := make([]string, 0, len(warnings))
		for _, warning := range warnings {
			notes = append(notes, warning.Error())
		}
		out["warnings"] = notes
	}
	writeJSON(w, http.StatusOK, out)
}

func manageStatus(err error) int {
	switch {
	case errors.Is(err, config.ErrStale), errors.Is(err, config.ErrDuplicate):
		return http.StatusConflict
	case errors.Is(err, config.ErrUnknown):
		return http.StatusNotFound
	case errors.Is(err, backend.ErrRegistryShutdown):
		return http.StatusServiceUnavailable
	default:
		// Anything else is a rejected declaration: an invalid name, or one naming
		// neither a command nor a URL.
		return http.StatusBadRequest
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
	// The issuer and the presence of an error, and never the code or the state. RFC 9207
	// makes iss conditional on the authorization server advertising support for it, and the
	// SDK refuses an authorization whose iss does not match what the metadata promised, so
	// whether it arrived at all is the first thing worth knowing about a consent that
	// completed and then went nowhere.
	slog.Info("oauth callback received",
		"iss", q.Get("iss"), "has_code", q.Get("code") != "",
		"provider_error", q.Get("error"), "provider_error_description", q.Get("error_description"))
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
		// Set per attempt, because it is cleared when a handshake ends: this handshake
		// cannot finish until the user has logged in to the provider and consented, so it
		// needs a budget sized for a person rather than for a dial.
		b.ExpectAuthorization()
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
		case authorizeUnfinished:
			// Left running deliberately. The discovery this is waiting on will finish and
			// publish its URL, and the next press answers from the pending check at the top
			// rather than starting anything, so pressing again is safe and picks it up.
			writeJSON(w, http.StatusAccepted, map[string]string{
				"status":  "starting",
				"message": "still setting up the authorization for " + name + "; press Authorize again in a moment",
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
	// authorizeUnusable: the handshake this reconnect started failed, so the stored grant
	// is not working and discarding it is justified.
	authorizeUnusable authorizeOutcome = iota
	authorizePending
	authorizeServing
	authorizeSuperseded
	// authorizeUnfinished: the wait ended without learning anything, because the request
	// was abandoned or the budget ran out. It says nothing about the grant, and must not be
	// treated as though it did: the retry that follows an unusable grant tears the backend
	// down, which would destroy an authorization the user is part way through.
	authorizeUnfinished
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
			// Which kind of done matters, because only one of them says anything about the
			// grant. This budget expiring is weak evidence against it: nothing appeared and
			// nothing failed, so one retry is worth spending. The request being cancelled is
			// no evidence at all, because it means the browser navigated away or gave up,
			// and retrying on that tears down an authorization the user may be part way
			// through and leaves the code they come back with matching nothing.
			if errors.Is(ctx.Err(), context.Canceled) {
				return "", authorizeUnfinished
			}
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
	switch {
	case errors.Is(err, backend.ErrDisabled):
		return http.StatusConflict
	case errors.Is(err, backend.ErrShutdown):
		// A daemon on its way out is unavailable, not broken.
		return http.StatusServiceUnavailable
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

// WithUnvectorized lets the status surface report how many catalog entries the embedding
// gateway has not embedded. A silently lexical-only ranking is otherwise indistinguishable
// from a working one until the results are visibly bad.
func (s *Server) WithUnvectorized(count func() int) *Server {
	s.unvectorized = count
	return s
}
