// Package web serves mcpd's loopback HTTP surface: the status page, the status
// API, the tool inspector, and the cross-origin guard the MCP endpoints share.
package web

import (
	"mime"
	"net/http"
)

// denyReason is sent on a cross-origin rejection. The default deny response is
// bare, and an operator reading a browser console needs to know which of the two
// guards refused the request.
const denyReason = "cross-origin request rejected: mcpd is a loopback daemon and serves no other origin"

// Guard holds the one cross-origin protection value every surface enforces. The
// MCP endpoints and the web routes share it rather than constructing one each,
// because two configurations drift and the drift is invisible until a route that
// should have been protected is not.
type Guard struct {
	protection *http.CrossOriginProtection
}

func NewGuard() *Guard {
	p := http.NewCrossOriginProtection()
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, denyReason, http.StatusForbidden)
	}))
	return &Guard{protection: p}
}

// Protect wraps h in the shared cross-origin policy. The MCP handlers are wrapped
// with this rather than through StreamableHTTPOptions.CrossOriginProtection, which
// is deprecated, applies nothing when nil, and ignores the deny handler.
func (g *Guard) Protect(h http.Handler) http.Handler { return g.protection.Handler(h) }

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
