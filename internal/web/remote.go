package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/config"
)

// WithRemote enables the panel's remote-relogin toggle. Without it the route
// is absent, exactly as the manager routes are without a manager.
func (s *Server) WithRemote(rc *Remote) *Server {
	s.remote = rc
	return s
}

type remoteToggleRequest struct {
	// Both optional: a request can set the advertised origin, change the
	// lifecycle, or both. Advertise applies first.
	Enabled   *bool   `json:"enabled"`
	Advertise *string `json:"advertise"`
}

// remoteToggle drives the lifecycle from the loopback panel. The response
// carries declared and running separately, because a declaration can stand
// while its listener could not start, and the panel must offer Disable then.
func (s *Server) remoteToggle(w http.ResponseWriter, r *http.Request) {
	var in remoteToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if in.Advertise != nil {
		if err := s.remote.SetAdvertise(*in.Advertise); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if in.Enabled != nil {
		if *in.Enabled {
			if _, err := s.remote.Enable(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		} else if err := s.remote.Disable(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"declared":  s.remote.Declared(),
		"running":   s.remote.Running(),
		"urls":      s.remote.URLs(),
		"advertise": s.remote.Advertise(),
	})
}

// RemoteWriter is the one config mutation the remote lifecycle needs.
type RemoteWriter interface {
	SetRemote(config.Remote) ([]error, error)
}

// Remote owns the LAN listener's lifecycle. One mutex serializes enable,
// disable and startup, so the listener, the token file and the config never
// describe different states.
type Remote struct {
	srv       *Server
	writer    RemoteWriter
	tokenPath string
	addr      string
	hostname  string

	mu        sync.Mutex
	ln        net.Listener
	hsrv      *http.Server
	token     string
	advertise string
	// declared is what config last said. It can be true while ln is nil: a
	// startup that could not bind or found no valid token leaves the
	// declaration standing, and Disable must still be able to retract it.
	declared bool
}

func NewRemote(s *Server, w RemoteWriter, tokenPath, addr, hostname string) *Remote {
	return &Remote{srv: s, writer: w, tokenPath: tokenPath, addr: addr, hostname: hostname}
}

// Enable binds first, so a failure has written nothing; then persists the
// token and the declaration, and only then serves. Enable while enabled hands
// back the current URLs without rotating anything.
func (r *Remote) Enable() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln != nil {
		return r.urlsLocked(), nil
	}
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		return nil, fmt.Errorf("bind remote listener: %w", err)
	}
	// A valid token can already exist (a startup that could not bind leaves
	// one behind with config still enabled), and a failed enable must not
	// destroy it: rollback restores it rather than deleting.
	prior, hadPrior := loadRemoteToken(r.tokenPath)
	tok, err := newRemoteToken()
	if err == nil {
		err = storeRemoteToken(r.tokenPath, tok)
	}
	if err == nil {
		_, err = r.writer.SetRemote(config.Remote{Enabled: true, Addr: r.addr, Advertise: r.advertise})
	}
	if err != nil {
		ln.Close()
		if hadPrior {
			if rerr := storeRemoteToken(r.tokenPath, prior); rerr != nil {
				slog.Warn("could not restore the prior remote token", "error", rerr)
			}
		} else {
			deleteRemoteToken(r.tokenPath)
		}
		return nil, err
	}
	r.serveLocked(ln, tok)
	r.declared = true
	return r.urlsLocked(), nil
}

// Disable persists the retraction first: a config write that fails leaves the
// listener running and the token valid, which is the state the file still
// describes. It retracts a standing declaration even when no listener is
// running, so a startup that could not bind cannot resurrect a listener the
// user turned off.
func (r *Remote) Disable() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.declared && r.ln == nil {
		return nil
	}
	if _, err := r.writer.SetRemote(config.Remote{Enabled: false, Addr: r.addr, Advertise: r.advertise}); err != nil {
		return err
	}
	r.stopLocked()
	deleteRemoteToken(r.tokenPath)
	r.declared = false
	return nil
}

// Apply reconciles the live lifecycle with a declaration the daemon did not
// write itself: startup, and a reload that adopted a hand-edited file. It
// never fails the daemon: a missing or malformed token and an unbindable
// address each log a token-free warning and leave only the remote listener
// off, with the declaration standing so Disable can retract it and the next
// restart retries. Config
// is the source here, so nothing is written back; the token follows the same
// rules as Disable. An empty Addr keeps the current one, which is
// how the daemon's default survives a declaration that never named an address.
func (r *Remote) Apply(decl config.Remote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rebind := decl.Addr != "" && decl.Addr != r.addr
	if rebind {
		r.addr = decl.Addr
	}
	adv, err := validateAdvertise(decl.Advertise)
	if err != nil {
		slog.Warn("ignoring an invalid advertised origin from config", "error", err)
		adv = ""
	}
	r.advertise = adv
	r.declared = decl.Enabled
	if !decl.Enabled {
		r.stopLocked()
		deleteRemoteToken(r.tokenPath)
		return
	}
	if r.ln != nil && !rebind {
		return
	}
	r.stopLocked()
	tok, ok := loadRemoteToken(r.tokenPath)
	if !ok {
		slog.Warn("remote access is declared enabled but no valid pairing token is stored; re-enable from the panel",
			"path", r.tokenPath)
		return
	}
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		slog.Warn("remote access is declared enabled but its address cannot be bound; serving without it",
			"addr", r.addr, "error", err)
		return
	}
	r.serveLocked(ln, tok)
}

func (r *Remote) serveLocked(ln net.Listener, tok string) {
	r.ln, r.token = ln, tok
	r.hsrv = &http.Server{
		Handler: r.srv.remoteHandler(func() string { return tok }),
		// This listener is reachable before authentication, so slow or idle
		// connections are bounded; the loopback server has no such exposure.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	go func(h *http.Server, l net.Listener) {
		if err := h.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("remote listener stopped", "error", err)
		}
	}(r.hsrv, ln)
	slog.Info("remote relogin surface serving", "addr", ln.Addr().String())
}

func (r *Remote) stopLocked() {
	if r.hsrv != nil {
		r.hsrv.Close()
	}
	r.ln, r.hsrv, r.token = nil, nil, ""
}

// Close stops a serving listener at daemon exit. The token file is kept:
// disable is the user's action, exit is not.
func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

// Running reports whether a listener is serving now.
func (r *Remote) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ln != nil
}

// Declared reports what config says, which can outlive a listener that could
// not start.
func (r *Remote) Declared() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.declared
}

// SetAdvertise validates, persists and adopts a reverse-proxy origin. An
// empty value clears it. Nothing is persisted when validation refuses.
func (r *Remote) SetAdvertise(raw string) error {
	canonical, err := validateAdvertise(raw)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.writer.SetRemote(config.Remote{Enabled: r.declared, Addr: r.addr, Advertise: canonical}); err != nil {
		return err
	}
	r.advertise = canonical
	return nil
}

// Advertise is the canonical reverse-proxy origin, empty when unset.
func (r *Remote) Advertise() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advertise
}

// URLs are the pairing URLs of a running listener, empty otherwise.
func (r *Remote) URLs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return nil
	}
	return r.urlsLocked()
}

func (r *Remote) urlsLocked() []string {
	bindHost := r.addr
	if h, _, err := net.SplitHostPort(r.addr); err == nil {
		bindHost = h
	}
	port := r.ln.Addr().(*net.TCPAddr).Port
	return pairingURLs(bindHost, port, r.token, interfaceAddrs(), r.hostname, r.advertise)
}

func interfaceAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if p, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, p.Unmap())
			}
		}
	}
	return out
}

// remoteRoutes is the complete LAN surface. A main-listener route reaches the
// LAN only by being added here, and TestRemoteForbiddenRoutesAbsent enumerates
// what must not appear.
func (s *Server) remoteRoutes() []route {
	return []route{
		{method: http.MethodGet, path: "/{$}", handler: s.remotePage},
		{method: http.MethodGet, path: "/api/status", handler: s.remoteStatus},
		{method: http.MethodGet, path: "/assets/", handler: http.FileServerFS(assetFS).ServeHTTP},
		{method: http.MethodPost, path: "/api/backends/{name}/authorize", mutates: true, handler: s.authorize},
		{method: http.MethodGet, path: "/oauth/callback", mutates: true, nonceGuarded: true,
			handler: s.oauthCallback},
		{method: http.MethodPost, path: "/api/callback", mutates: true, handler: s.remotePastedCallback},
	}
}

// remoteHandler assembles the remote mux. Ordering, outermost first: the
// private-peer gate (a public source sees 403 for every path, callback
// included), the shared cross-origin policy, then per-route token auth and the
// method-plus-JSON rule.
func (s *Server) remoteHandler(token func() string) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.remoteRoutes() {
		if rt.nonceGuarded {
			// The callback authenticates by its one-time state nonce, exactly as
			// on the loopback listener; a token gate would break the address-bar
			// completion path for no protection the nonce does not already give.
			mux.Handle(rt.path, guardMethod(s.guard, rt, rt.handler))
			continue
		}
		mux.Handle(rt.path, requireRemoteToken(token, guardMethod(s.guard, rt, rt.handler)))
	}
	return requirePrivatePeer(s.guard.Protect(mux))
}

func requirePrivatePeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !privatePeer(r.RemoteAddr, localPrefixes()) {
			deny(w, "the remote surface serves the local network only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireRemoteToken is the pairing gate. A request carrying ?token= trades a
// matching token for the cookie and a redirect that strips the token from the
// URL and the browser history; everything else must present the cookie. Both
// comparisons are constant-time, and an empty expected token never matches.
func requireRemoteToken(token func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := token()
		if q := r.URL.Query().Get("token"); q != "" {
			if want == "" || subtle.ConstantTimeCompare([]byte(q), []byte(want)) != 1 {
				deny(w, "invalid token")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: "mcpd_remote", Value: want, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie("mcpd_remote")
		if err != nil || want == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) != 1 {
			deny(w, "this surface requires the pairing token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type remoteBackend struct {
	Name      string `json:"name"`
	NeedsAuth bool   `json:"needs_auth"`
	State     string `json:"state"`
	// Label and Tone are the state as the main panel renders it, from the same
	// classifier, so the two pages never describe one backend differently.
	Label string `json:"label"`
	Tone  string `json:"tone"`
}

// remoteStatus lists only what the relogin page needs: OAuth-backed backends
// and whether each is waiting on an authorization. No tool data, no specs, no
// config, and no authorization URL: that travels in the authorize action's
// response, as on the loopback panel.
func (s *Server) remoteStatus(w http.ResponseWriter, _ *http.Request) {
	out := []remoteBackend{}
	for _, name := range s.reg.Names() {
		b, ok := s.reg.Get(name)
		if !ok || !b.UsesOAuth() {
			continue
		}
		h := b.Health()
		label, tone := classify(h)
		out = append(out, remoteBackend{
			Name:      name,
			NeedsAuth: h.State == backend.StateNeedsAuth,
			State:     string(h.State),
			Label:     label,
			Tone:      tone,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type pastedCallback struct {
	URL string `json:"url"`
}

// remotePastedCallback completes an authorization from the dead-redirect URL
// the user copied. It ends in the same Deliver the callback routes use, so the
// state nonce is consumed once no matter which path carried it.
func (s *Server) remotePastedCallback(w http.ResponseWriter, r *http.Request) {
	var in pastedCallback
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Query().Get("state") == "" || u.Query().Get("code") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "that does not look like a callback URL; it must carry state and code parameters"})
		return
	}
	q := u.Query()
	if err := s.oauth.Deliver(q.Get("state"), q.Get("code"), q.Get("iss")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this callback matches no outstanding authorization request"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
}

func (s *Server) remotePage(w http.ResponseWriter, r *http.Request) {
	adv := ""
	if s.remote != nil {
		adv = s.remote.Advertise()
	}
	render(w, "remote.html", struct{ Addr, Advertise string }{r.Host, adv})
}
