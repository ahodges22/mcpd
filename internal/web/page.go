package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/catalog"
)

//go:embed templates
var templateFS embed.FS

// pages are the only path a backend-derived string takes into markup, and
// html/template's contextual escaping is what makes that safe.
var pages = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// namedShare is the share of the catalog a bus segment must hold before it carries its
// own name. Below it the label would not fit, and clipping a name reads as corruption.
const namedShare = 7

type backendStatus struct {
	Name string `json:"name"`
	backend.Health
	// Label is the state as rendered, which is not always the state as recorded.
	Label string `json:"label"`
	// Tone is the visual class of the state: one lamp colour per distinguishable
	// condition, including the one the State enum cannot express.
	Tone         string `json:"-"`
	CatalogError string `json:"catalog_error,omitempty"`
	OAuth        bool   `json:"oauth"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
	// Grow is this backend's share of the catalog, as a flex weight for the bus. It is
	// computed here rather than measured in the browser so the bar needs no layout pass.
	Grow int `json:"-"`
	// Named reports whether the segment is wide enough to carry its own name.
	Named bool `json:"-"`
	// Alt steps the segment's brightness down. Every serving backend shares one hue,
	// because hue means state here and nothing else, so alternating brightness is what
	// lets twelve adjacent segments be counted rather than read as one long block.
	Alt bool `json:"-"`

	tokenExp time.Time
}

type statusView struct {
	Backends  []backendStatus `json:"backends"`
	ToolCount int             `json:"tool_count"`
	// Unvectorized is how many catalog entries the embedding gateway has not embedded.
	// A negative value means no gateway is configured, so the surface says nothing rather
	// than reporting a zero that would read as "fully embedded".
	Unvectorized int `json:"unvectorized"`
	// Serving is how many backends are answering, which is the number the page leads with.
	Serving int `json:"serving"`
	// Addr is the address this request arrived on, already checked against the loopback
	// host rule, so the page can name the endpoint clients are pointed at.
	Addr string `json:"-"`
	// Segments is the bus, ordered by share so the bar reads as a composition. The table
	// stays in declaration order, because that is a lookup rather than a comparison.
	Segments []backendStatus `json:"-"`
	// Attention is every backend that wants something from the user, lifted out of the
	// table so a needed action is never something to go hunting for.
	Attention []backendStatus `json:"-"`
	// Remote is the LAN relogin listener's state, for the panel only.
	Remote remoteView `json:"-"`
}

// remoteView is what the panel needs to render the remote-relogin section:
// whether the toggle exists at all, what config declares, whether a listener
// is actually serving, and the pairing URLs when it is.
type remoteView struct {
	Available bool
	Declared  bool
	Running   bool
	URLs      []pairURL
	Advertise string
}

// pairURL is one pairing URL split where the secret starts, so the page can
// set the address bright and the token dim: the address is what a person
// reads, the token is what they carry.
type pairURL struct {
	Full string
	Base string
	Key  string
}

func splitPairURL(full string) pairURL {
	base, key, found := strings.Cut(full, "?token=")
	if !found {
		return pairURL{Full: full, Base: full}
	}
	return pairURL{Full: full, Base: base, Key: "?token=" + key}
}

type toolView struct {
	ID          string
	Tool        string
	Description string
	Schema      string
	// Confirm names why a tool needs a confirming action, and is empty when it
	// declares itself read-only.
	Confirm string
	// Badge is the same fact at label length, because "carrying no read-only annotation"
	// beside a tool's name is longer than the name.
	Badge string
}

type inspectView struct {
	Name  string
	Addr  string
	Tools []toolView
}

func (s *Server) snapshot() statusView {
	health := s.reg.Health()
	errs := s.cat.Errors()
	out := statusView{Backends: make([]backendStatus, 0, len(health)), ToolCount: len(s.cat.Entries()), Unvectorized: -1}
	if s.unvectorized != nil {
		out.Unvectorized = s.unvectorized()
	}
	for _, name := range s.reg.Names() {
		h := health[name]
		label, tone := classify(h)
		// Read from the declaration rather than inferred from the auth note: a backend
		// declared with oauth that has not yet produced a note still needs the authorize
		// action, and inferring it from the note withheld the action from exactly the
		// backend that had never once authorized.
		oauth := false
		if b, ok := s.reg.Get(name); ok {
			oauth = b.UsesOAuth()
		}
		exp, _ := s.oauth.TokenExpiry(name)
		entry := backendStatus{
			Name:         name,
			Health:       h,
			Label:        label,
			Tone:         tone,
			CatalogError: errs[name],
			OAuth:        oauth,
			TokenExpiry:  s.tokenExpiry(name),
			tokenExp:     exp,
		}
		if h.State == backend.StateUp {
			out.Serving++
		}
		if entry.Wants() != "" {
			out.Attention = append(out.Attention, entry)
		}
		out.Backends = append(out.Backends, entry)
	}
	out.Segments = bus(out.Backends)
	if s.remote != nil {
		urls := s.remote.URLs()
		pairs := make([]pairURL, 0, len(urls))
		for _, u := range urls {
			pairs = append(pairs, splitPairURL(u))
		}
		out.Remote = remoteView{
			Available: true,
			Declared:  s.remote.Declared(),
			Running:   s.remote.Running(),
			URLs:      pairs,
			Advertise: s.remote.Advertise(),
		}
	}
	return out
}

// bus lays the backends out as one bar whose segments are proportional to what each
// contributes to the catalog. A backend serving nothing gets no width at all and renders
// as a notch, so the bar shows both what the catalog holds and, in its gaps, what is
// missing. Ordered by share, largest first, because a proportion is read by comparison.
func bus(all []backendStatus) []backendStatus {
	total := 0
	for _, b := range all {
		if b.Serving() {
			total += b.ToolCount
		}
	}
	out := make([]backendStatus, len(all))
	copy(out, all)
	for i := range out {
		if !out[i].Serving() || total == 0 {
			continue
		}
		share := 100 * out[i].ToolCount / total
		// Floored at one: a backend that is serving is present, and rounding it to
		// nothing would render it as the notch that means the opposite.
		out[i].Grow = max(share, 1)
		out[i].Named = share >= namedShare
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Serving() != out[j].Serving() {
			return out[i].Serving()
		}
		return out[i].ToolCount > out[j].ToolCount
	})
	// Alternated after the sort, so the step falls between neighbours as drawn.
	for i := range out {
		out[i].Alt = i%2 == 1
	}
	return out
}

// Serving reports whether this backend is answering, which is the one distinction the
// bus draws: a segment or a notch.
func (b backendStatus) Serving() bool { return b.State == backend.StateUp }

// Wants is what this backend needs from the user, in the interface's own voice, or
// nothing at all when it needs nothing. A backend the user turned off wants nothing, and
// neither does one that has simply not been dialled yet, so neither raises an alarm.
func (b backendStatus) Wants() string {
	switch {
	case b.State == backend.StateNeedsAuth:
		return b.Name + " is waiting for you to authorize it"
	case b.Tone != "fault":
		return ""
	case b.OAuth:
		return b.Name + " is refusing the connection, and it authorizes with OAuth"
	default:
		return b.Name + " is not answering"
	}
}

// FixLabel and FixPath are the one action that addresses what the backend wants. An OAuth
// backend is offered authorization even when it reports as down, because the authorize
// route reconnects first and a 401 is how a missing grant presents.
func (b backendStatus) FixLabel() string {
	if b.OAuth {
		return "Authorize"
	}
	return "Reconnect"
}

func (b backendStatus) FixPath() string {
	if b.OAuth {
		return "/api/backends/" + b.Name + "/authorize"
	}
	return "/api/backends/" + b.Name + "/reconnect"
}

// Trouble is what the backend reported, preferring its own error over the catalog's
// restatement of it. It is rendered verbatim: it is the only signal that says what
// actually happened, and paraphrasing it would cost the user the diagnosis.
func (b backendStatus) Trouble() string {
	if b.LastErr != "" {
		return b.LastErr
	}
	return b.CatalogError
}

// Age is how long ago the tool list was read, which answers the question a timestamp
// makes the reader do arithmetic for: is this stale? The exact time stays available as
// the element's title.
func (b backendStatus) Age() string {
	if b.LastRefresh.IsZero() {
		return "never read"
	}
	return "read " + since(b.LastRefresh) + " ago"
}

// TokenNote reports how long the stored grant remains valid, so a token about to lapse is
// distinguishable from one the daemon has stopped refreshing. Only the expiry is read:
// the token itself never reaches a response or a page.
func (b backendStatus) TokenNote() string {
	if b.tokenExp.IsZero() {
		return ""
	}
	if d := time.Until(b.tokenExp); d > 0 {
		return "token valid for " + short(d)
	}
	return "token expired " + since(b.tokenExp) + " ago"
}

func since(t time.Time) string { return short(time.Since(t)) }

// short renders a duration at one significant unit, because the page reports an age
// rather than measuring an interval.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// tokenExpiry renders the stored token's expiry, so the surface distinguishes a
// token about to lapse from one the daemon has stopped refreshing. Only the expiry
// is read: the token itself never reaches a response or a page.
func (s *Server) tokenExpiry(name string) string {
	exp, ok := s.oauth.TokenExpiry(name)
	if !ok {
		return ""
	}
	return exp.Format(time.RFC3339)
}

// classify renders a health state for a human and gives it a lamp colour. StateDown
// cannot distinguish a backend that was never dialled from one that was and failed, so a
// zero refresh time with no error reads as the former: on a fresh daemon every backend
// would otherwise look like an alarming failure. Both outputs come from here so the label
// and the colour cannot drift apart.
func classify(h backend.Health) (label, tone string) {
	switch h.State {
	case backend.StateUp:
		return "Serving", "up"
	case backend.StateDisabled:
		return "Turned off", "off"
	case backend.StateNeedsAuth:
		return "Needs authorizing", "wait"
	}
	if h.LastRefresh.IsZero() && h.LastErr == "" {
		return "Not dialled yet", "cold"
	}
	return "Not answering", "fault"
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	view := s.snapshot()
	// The Host header, already checked against the loopback rule by the guard this
	// handler sits behind, so the page can name the endpoint without being told it.
	view.Addr = r.Host
	render(w, "status.html", view)
}

func (s *Server) inspectPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.reg.Get(name); !ok {
		http.Error(w, "unknown backend", http.StatusNotFound)
		return
	}
	view := inspectView{Name: name, Addr: r.Host}
	for _, e := range s.cat.Entries() {
		if e.Server != name {
			continue
		}
		reason, badge := confirmReason(e.Annotations)
		view.Tools = append(view.Tools, toolView{
			ID:          e.ID,
			Tool:        e.Tool,
			Description: e.Description,
			Schema:      indentSchema(e),
			Confirm:     reason,
			Badge:       badge,
		})
	}
	render(w, "inspect.html", view)
}

// confirmReason reports why invoking a tool asks for a confirming action, treating
// an absent annotation as unsafe: a backend that says nothing about a tool is not
// evidence the tool is harmless. It is a guard against the user's own misclick and
// not a security control, which is why nothing on the invoke path consults it.
//
// The badge is the same judgement at label length. Both come out of one switch so the
// short form on the page can never disagree with the reason in the confirmation.
func confirmReason(a *mcp.ToolAnnotations) (reason, badge string) {
	switch {
	case a == nil:
		return "carrying no read-only annotation", "unannotated"
	case a.ReadOnlyHint:
		return "", ""
	case a.DestructiveHint == nil || *a.DestructiveHint:
		return "destructive", "destructive"
	default:
		return "not read-only", "writes"
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
	// Script and style are served from /assets, never inline, so 'self' is enough and
	// inline script stays absent where the asset test can see it. frame-ancestors
	// closes clickjacking of the one-click disable, reconnect and re-index buttons,
	// which neither the Host nor the origin check covers.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
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
