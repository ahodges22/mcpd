// Package web serves mcpd's loopback HTTP surface: the status page, the status
// API, the tool inspector, and the cross-origin guard the MCP endpoints share.
package web

import (
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// denyReason is sent on a cross-origin rejection. The default deny response is
// bare, and an operator reading a browser console needs to know which of the two
// guards refused the request.
const denyReason = "cross-origin request rejected: mcpd is a loopback daemon and serves no other origin"

// rebindReason is sent when the Host header names something other than loopback,
// which is what a rebound name looks like by the time it reaches this daemon.
const rebindReason = "request rejected: the Host header is not a loopback address"

// Guard holds the one cross-origin protection value every surface enforces. The
// MCP endpoints and the web routes share it rather than constructing one each,
// because two configurations drift and the drift is invisible until a route that
// should have been protected is not.
type Guard struct {
	protection *http.CrossOriginProtection
}

func NewGuard() *Guard {
	g := &Guard{protection: http.NewCrossOriginProtection()}
	g.protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deny(w, denyReason)
	}))
	return g
}

// Protect wraps h in the shared cross-origin policy. The MCP handlers are wrapped
// with this rather than through StreamableHTTPOptions.CrossOriginProtection, which
// is deprecated, applies nothing when nil, and ignores the deny handler.
func (g *Guard) Protect(h http.Handler) http.Handler { return g.protection.Handler(h) }

// RequireLoopbackHost is the DNS-rebinding check, and it is not redundant with the
// cross-origin policy: CrossOriginProtection reads only Sec-Fetch-Site and Origin,
// and a browser on a rebound name believes it is same-origin, so it sends
// Sec-Fetch-Site: same-origin and passes every other guard here, JSON content type
// included. Only the Host header still names the attacker. The MCP endpoints get
// the equivalent check from the SDK's own handler, which is why
// DisableLocalhostProtection is left alone; this is the web routes' half.
func (g *Guard) RequireLoopbackHost(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			deny(w, rebindReason)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	// A host name is case-insensitive, and a single trailing dot is the legal absolute
	// form, so the daemon's own address must not be refused in either spelling. Only
	// one dot is stripped, because anything beyond that is not a host name.
	name = strings.TrimSuffix(name, ".")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(name)
	return err == nil && addr.IsLoopback()
}

func deny(w http.ResponseWriter, reason string) {
	http.Error(w, reason, http.StatusForbidden)
}

// guardMethod is the POST-plus-JSON rule. Cross-origin protection cannot do this
// job: GET, HEAD and OPTIONS are safe methods to it and are always allowed, so
// only this keeps a state change out of reach of navigation, an image load, or a
// cross-origin form submission, which cannot set a JSON content type.
//
// Task 10's OAuth callback is the single documented exemption: it is necessarily a
// top-level browser GET, and its one-time state nonce protects it instead.
func guardMethod(rt route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != rt.method {
			w.Header().Set("Allow", rt.method)
			http.Error(w, "method not allowed: "+rt.path+" accepts "+rt.method, http.StatusMethodNotAllowed)
			return
		}
		if rt.mutates && !isJSON(r.Header.Get("Content-Type")) {
			http.Error(w, "a state change requires a JSON content type", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isJSON(header string) bool {
	kind, _, err := mime.ParseMediaType(header)
	return err == nil && kind == "application/json"
}
