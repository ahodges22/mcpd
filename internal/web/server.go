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
)

//go:embed assets
var assetFS embed.FS

// Server is the loopback web surface. It is constructed without a listener, so it
// is testable on its own; cmd/mcpd mounts Handler on its own mux.
type Server struct {
	reg   *backend.Registry
	cat   *catalog.Catalog
	guard *Guard
	mux   *http.ServeMux
}

// route is one registered path. Patterns carry no method, because the method check
// belongs in one reviewable place rather than being implied by the mux's routing.
type route struct {
	method  string
	path    string
	mutates bool // changes daemon or backend state, so it is POST plus JSON only
	handler http.HandlerFunc
}

func New(reg *backend.Registry, cat *catalog.Catalog, g *Guard) *Server {
	s := &Server{reg: reg, cat: cat, guard: g, mux: http.NewServeMux()}
	for _, rt := range s.routes() {
		s.mux.Handle(rt.path, guardMethod(rt, rt.handler))
	}
	return s
}

// Handler is the web surface behind the shared cross-origin guard.
func (s *Server) Handler() http.Handler { return s.guard.Protect(s.mux) }

func (s *Server) routes() []route {
	return []route{
		{method: http.MethodGet, path: "/", handler: s.statusPage},
		{method: http.MethodGet, path: "/api/status", handler: s.statusAPI},
		{method: http.MethodGet, path: "/inspect/{name}", handler: s.inspectPage},
		{method: http.MethodGet, path: "/assets/", handler: http.FileServerFS(assetFS).ServeHTTP},
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

// runTransition waits a bounded time for an operation that cannot be cancelled.
// Registry.Enable, Disable and Reconnect take no context and can take none, because
// gate.Lock has no cancellable variant. So on expiry the response gives up while
// the work continues to completion, rather than the work being abandoned.
func runTransition(w http.ResponseWriter, timeout time.Duration, op func() error) {
	done := make(chan error, 1)
	go func() { done <- op() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, backend.ErrDisabled) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case <-timer.C:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "in-progress",
			"error":  "the transition cannot be cancelled and is still running; it will finish in the background",
		})
	}
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
