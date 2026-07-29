package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/catalog"
)

//go:embed templates
var templateFS embed.FS

// pages are the only path a backend-derived string takes into markup, and
// html/template's contextual escaping is what makes that safe.
var pages = template.Must(template.ParseFS(templateFS, "templates/*.html"))

type backendStatus struct {
	Name string `json:"name"`
	backend.Health
	// Label is the state as rendered, which is not always the state as recorded.
	Label        string `json:"label"`
	CatalogError string `json:"catalog_error,omitempty"`
	OAuth        bool   `json:"oauth"`
}

type statusView struct {
	Backends  []backendStatus `json:"backends"`
	ToolCount int             `json:"tool_count"`
}

type toolView struct {
	ID          string
	Tool        string
	Description string
	Schema      string
	// Confirm names why a tool needs a confirming action, and is empty when it
	// declares itself read-only.
	Confirm string
}

type inspectView struct {
	Name  string
	Tools []toolView
}

func (s *Server) snapshot() statusView {
	health := s.reg.Health()
	errs := s.cat.Errors()
	out := statusView{Backends: make([]backendStatus, 0, len(health)), ToolCount: len(s.cat.Entries())}
	for _, name := range s.reg.Names() {
		h := health[name]
		out.Backends = append(out.Backends, backendStatus{
			Name:         name,
			Health:       h,
			Label:        stateLabel(h),
			CatalogError: errs[name],
			OAuth:        h.AuthNote != "",
		})
	}
	return out
}

// stateLabel renders a health state for a human. StateDown cannot distinguish a
// backend that was never dialled from one that was and failed, so a zero refresh
// time with no error reads as the former: on a fresh daemon every backend would
// otherwise look like an alarming failure.
func stateLabel(h backend.Health) string {
	switch h.State {
	case backend.StateUp:
		return "Up"
	case backend.StateDisabled:
		return "Disabled"
	case backend.StateNeedsAuth:
		return "Needs auth"
	}
	if h.LastRefresh.IsZero() && h.LastErr == "" {
		return "Not yet connected"
	}
	return "Down"
}

func (s *Server) statusPage(w http.ResponseWriter, _ *http.Request) {
	render(w, "status.html", s.snapshot())
}

func (s *Server) inspectPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.reg.Get(name); !ok {
		http.Error(w, "unknown backend", http.StatusNotFound)
		return
	}
	view := inspectView{Name: name}
	for _, e := range s.cat.Entries() {
		if e.Server != name {
			continue
		}
		view.Tools = append(view.Tools, toolView{
			ID:          e.ID,
			Tool:        e.Tool,
			Description: e.Description,
			Schema:      indentSchema(e),
			Confirm:     confirmReason(e.Annotations),
		})
	}
	render(w, "inspect.html", view)
}

// confirmReason reports why invoking a tool asks for a confirming action, treating
// an absent annotation as unsafe: a backend that says nothing about a tool is not
// evidence the tool is harmless. It is a guard against the user's own misclick and
// not a security control, which is why nothing on the invoke path consults it.
func confirmReason(a *mcp.ToolAnnotations) string {
	switch {
	case a == nil:
		return "carrying no read-only annotation"
	case a.ReadOnlyHint:
		return ""
	case a.DestructiveHint == nil || *a.DestructiveHint:
		return "destructive"
	default:
		return "not read-only"
	}
}

func indentSchema(e catalog.Entry) string {
	if len(e.Schema) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, e.Schema, "", "  "); err != nil {
		return string(e.Schema)
	}
	return buf.String()
}

// render buffers the page, so a template failure cannot append an error to a
// partly written 200.
func render(w http.ResponseWriter, page string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, page, data); err != nil {
		http.Error(w, "render "+page+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Script and style are served from /assets, never inline, so 'self' is enough
	// and inline script stays absent where the asset test can see it.
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
	w.Write(buf.Bytes())
}

// RefreshedAt renders the refresh time, or nothing at all when the backend has
// never been read: a zero time formats as the year 1 and reads as a real reading.
func (b backendStatus) RefreshedAt() string {
	if b.LastRefresh.IsZero() {
		return ""
	}
	return b.LastRefresh.Format(time.RFC3339)
}
