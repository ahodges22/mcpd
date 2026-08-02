// Package install points coding-agent clients at mcpd and takes them back off it again.
//
// Every edit here is a text splice on the file's current bytes, never a parse and
// re-serialize. That is not a style preference. These files are hand-authored, they carry
// comments and credentials and key orders the user chose, and a revert has to be able to
// leave the file byte-for-byte as it found it while still preserving edits the user made
// after installing. Re-serializing would reformat everything and make both of those
// impossible at once.
package install

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ahodges22/mcpd/internal/atomicfile"
)

// ErrConflict reports that a region mcpd wrote is no longer as mcpd wrote it, so a revert
// would have to guess whose version wins. Refusing is recoverable; a silent wrong merge
// is not.
var ErrConflict = errors.New("a region mcpd owns was changed by hand")

// ErrNotInstalled reports that there is no record of mcpd having written to this client.
var ErrNotInstalled = errors.New("mcpd is not installed for this client")

// Mode is which endpoint a client is pointed at.
type Mode string

const (
	// Passthrough serves the whole catalog under mcp__<server>__<tool> names, for a client
	// with its own tool search. Stacking the facade behind such a client ranks worse than
	// either layer alone and saves nothing: measured over a 583-tool catalog, one such
	// client spent 40,071 tokens through the facade against 40,129 natively.
	Passthrough Mode = "passthrough"
	// Search serves the three-tool facade, for a client that would otherwise load every
	// tool schema up front.
	Search Mode = "search"
)

// ServerName is the key mcpd installs itself under in every client. It is also the prefix
// the receipt addresses are built from, so it appears in exactly one place.
const ServerName = "mcpd"

// StashKey is where a client's existing MCP block is moved to. It is a sibling key rather
// than a deletion, so nothing the user declared is ever destroyed, and it is renamed as one
// unit so every byte inside it is left exactly as it was.
const StashKey = "_mcpd_stashed"

// Client is one client's configuration file and how mcpd edits it.
type Client struct {
	// Name is what the user types: mcpd install --client codex.
	Name string
	// Path is the file, already resolved against the home directory.
	Path string
	// Mode is the endpoint this client is pointed at, decided by whether it has native
	// tool search.
	Mode Mode
	// edit is the format-specific work. Both JSON and TOML clients reduce to the same
	// three questions: what text do I add, what do I rename, and what did I find.
	edit editor
}

// editor is a client format's splicing rules.
type editor interface {
	// plan returns the edits to make against these bytes, in order, plus any text lifted out
	// of the file that only the receipt will hold. A format with comments returns none of the
	// latter: it can leave the text in place, inert, where the user can still read it.
	plan(c Client, body, endpoint string) ([]edit, []string, string, error)
	// revert returns the edits that undo a receipt against the current bytes. It is
	// format-specific because only one of the two formats can use the plain inverse.
	revert(c Client, body string, rec Receipt) ([]edit, error)
	// validate reports whether these bytes are still something the client can read.
	validate(body string) error
}

// verify refuses to write a file the client could no longer read.
//
// This is the only check here that does not depend on this package's own arithmetic. Every
// other guard re-reads the offsets that produced the bytes, so a mistake in them validates
// itself: an append addressed against the wrong length spliced a table into the middle of a
// key and the result was written out, leaving Codex unable to load any server in the file. A
// parser is independent evidence.
//
// The original is parsed too, and only so the refusal can say which of the two happened. A
// file that was already unparseable is the user's to fix, and mcpd must not claim it broke it.
func (c Client) verify(before, after string) error {
	err := c.edit.validate(after)
	if err == nil {
		return nil
	}
	if c.edit.validate(before) != nil {
		return fmt.Errorf("%s could not be parsed before this change either, so it is not mcpd's to repair: %w", c.Path, err)
	}
	return fmt.Errorf("refusing to write %s because the result would not parse: %w", c.Path, err)
}

// atEnd asks for the end of the body as it stands when the edit is applied. An absolute
// offset cannot express that: earlier edits change the length, so an append addressed by the
// original length lands inside the file. That is not hypothetical. It spliced a table into
// the middle of an args = [...] line and left Codex unable to load any server at all.
const atEnd = -1

// edit is one reversible text change. Every edit is either an insertion (from is empty) or
// a one-for-one replacement, and both invert by construction.
type edit struct {
	// Address names the edit for the receipt and for a conflict message, so a refusal can
	// say which key it is about rather than only which file.
	Address string
	// From is the exact text replaced, or empty for an insertion.
	From string
	// To is the exact text written.
	To string
	// At is where an insertion goes: the offset in the original body. Ignored for a
	// replacement.
	At int
	// Note is what the plan tells the user about this edit.
	Note string
}

// Receipt is what mcpd wrote to one client, so a revert can undo exactly that and nothing
// else. It lives beside the daemon's other state rather than inside the client's file: a
// client is entitled to reject a key it does not know.
type Receipt struct {
	Client    string `json:"client"`
	Path      string `json:"path"`
	Endpoint  string `json:"endpoint"`
	InstallAt string `json:"installed_at"`
	// OriginalHash distinguishes an interrupted install from a later conflicting edit.
	OriginalHash string `json:"original_hash,omitempty"`
	// Edits are in the order they were applied. A revert walks them backwards.
	Edits []edit `json:"edits"`
	// Displaced is the text the install took out of the client's file, held here because the
	// file itself must not carry a key mcpd invented. A client is entitled to reject a key it
	// does not know and one does: OpenCode validates its configuration against a schema and
	// refuses to start on an unrecognised top-level key, so the stash that used to live under
	// `_mcpd_stashed` inside the file took the whole client down rather than only its servers.
	//
	// Empty for a format whose displaced text stays in the file, commented out, and empty in a
	// receipt written before this field existed. Revert distinguishes those two cases from a
	// receipt that genuinely displaced nothing by looking for the old key in the file.
	Displaced string `json:"displaced,omitempty"`
}

// Plan is what an install or a revert would do to a client file. It is what --dry-run prints,
// and it is also what apply consumes, so the file change shown is the file change done.
type Plan struct {
	Client   string
	Path     string
	Endpoint string
	// Notes describe each change in the user's terms.
	Notes []string
	// Warnings are things that will still be true after the change.
	Warnings []string
	edits    []edit
	// body is the bytes the edits were computed against. Apply re-reads and refuses if
	// they have moved on, so a plan can never be applied to a file it did not see.
	body string
	// displaced is text the edits lift out of the file for the receipt to hold.
	displaced string
}

// Empty reports that there is nothing to do.
func (p Plan) Empty() bool { return len(p.edits) == 0 }

// Clients are the four clients this machine runs, with the endpoint each one gets.
func Clients(home string) []Client {
	return []Client{
		// Native tool search, so both of these take the whole catalog.
		{Name: "claude", Path: filepath.Join(home, ".claude.json"), Mode: Passthrough,
			edit: jsonEditor{container: "mcpServers", entry: map[string]any{"type": "http"}}},
		{Name: "codex", Path: filepath.Join(home, ".codex", "config.toml"), Mode: Passthrough,
			edit: tomlEditor{}},
		// No native tool search, so these take the facade rather than 583 schemas.
		{Name: "cursor", Path: filepath.Join(home, ".cursor", "mcp.json"), Mode: Search,
			edit: jsonEditor{container: "mcpServers", entry: map[string]any{"type": "http"}}},
		{Name: "opencode", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), Mode: Search,
			edit: jsonEditor{container: "mcp", entry: map[string]any{"type": "remote", "enabled": true}}},
	}
}

// Lookup finds a client by the name the user types.
func Lookup(home, name string) (Client, error) {
	for _, c := range Clients(home) {
		if c.Name == name {
			return c, nil
		}
	}
	names := make([]string, 0, 4)
	for _, c := range Clients(home) {
		names = append(names, c.Name)
	}
	return Client{}, fmt.Errorf("unknown client %q; known clients are %s", name, strings.Join(names, ", "))
}

// Endpoint is the URL this client is pointed at.
func (c Client) Endpoint(addr string) string {
	return "http://" + addr + "/mcp/" + string(c.Mode)
}

// PlanInstall works out what pointing this client at mcpd would change.
func (c Client) PlanInstall(addr string) (Plan, error) {
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return Plan{}, fmt.Errorf("read %s: %w", c.Path, err)
	}
	endpoint := c.Endpoint(addr)
	edits, warnings, displaced, err := c.edit.plan(c, string(raw), endpoint)
	if err != nil {
		return Plan{}, fmt.Errorf("%s: %w", c.Path, err)
	}
	p := Plan{
		Client: c.Name, Path: c.Path, Endpoint: endpoint,
		Warnings: warnings, edits: edits, body: string(raw), displaced: displaced,
	}
	for _, e := range edits {
		p.Notes = append(p.Notes, e.Note)
	}
	return p, nil
}

// Apply performs a planned install and records what it did. It re-reads the file first:
// a plan computed against bytes that have since changed would splice into the wrong place.
func (c Client) Apply(state string, p Plan) error {
	if p.Empty() {
		return nil
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", c.Path, err)
	}
	if string(raw) != p.body {
		return fmt.Errorf("%s changed since it was inspected; run the plan again", c.Path)
	}
	body, err := applyEdits(string(raw), p.edits)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Path, err)
	}
	// Before the backup, so a refusal leaves no trace at all: nothing was changed, and a
	// stray backup beside an untouched file only invites a needless restore.
	if err := c.verify(string(raw), body); err != nil {
		return err
	}
	if err := writeReceipt(state, Receipt{
		Client: c.Name, Path: c.Path, Endpoint: p.Endpoint,
		InstallAt: time.Now().UTC().Format(time.RFC3339), Edits: p.edits,
		Displaced: p.displaced, OriginalHash: contentHash(raw),
	}); err != nil {
		return err
	}
	if err := backup(c.Path, raw); err != nil {
		return err
	}
	if err := write(c.Path, body); err != nil {
		return err
	}
	return nil
}

// PlanRevert works out what taking this client back off mcpd would change, refusing when a
// region mcpd wrote is no longer as it wrote it. A receipt whose install never reached the
// client file is removed and produces an empty plan.
func (c Client) PlanRevert(state string) (Plan, error) {
	rec, err := readReceipt(state, c.Name)
	if err != nil {
		return Plan{}, err
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return Plan{}, fmt.Errorf("read %s: %w", c.Path, err)
	}
	if rec.OriginalHash != "" && contentHash(raw) == rec.OriginalHash {
		if err := os.Remove(receiptPath(state, c.Name)); err != nil {
			return Plan{}, fmt.Errorf("remove stale receipt: %w", err)
		}
		return Plan{Client: c.Name, Path: c.Path, Endpoint: rec.Endpoint, body: string(raw)}, nil
	}
	edits, err := c.edit.revert(c, string(raw), rec)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Client: c.Name, Path: c.Path, Endpoint: rec.Endpoint, edits: edits, body: string(raw)}
	for _, e := range edits {
		p.Notes = append(p.Notes, e.Note)
	}
	return p, nil
}

// textualInverse undoes each recorded edit by putting back what it replaced, matched against
// the file's current bytes so an unrelated later edit elsewhere is simply not looked at. It
// walks backwards, because a later edit may sit inside the text an earlier one produced.
func textualInverse(c Client, body string, rec Receipt) ([]edit, error) {
	var out []edit
	for i := len(rec.Edits) - 1; i >= 0; i-- {
		e := rec.Edits[i]
		if n := strings.Count(body, e.To); n != 1 {
			return nil, fmt.Errorf("%w: %s in %s (found %d times, want 1)",
				ErrConflict, e.Address, c.Path, n)
		}
		undo := edit{Address: e.Address, From: e.To, To: e.From, Note: "put " + e.Address + " back"}
		if e.From == "" {
			undo.Note = "remove " + e.Address
		}
		body = strings.Replace(body, undo.From, undo.To, 1)
		out = append(out, undo)
	}
	return out, nil
}

// Revert performs a planned revert and drops the receipt.
func (c Client) Revert(state string, p Plan) error {
	if p.Empty() {
		return nil
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", c.Path, err)
	}
	if string(raw) != p.body {
		return fmt.Errorf("%s changed since it was inspected; run the plan again", c.Path)
	}
	body, err := applyEdits(string(raw), p.edits)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Path, err)
	}
	if err := c.verify(string(raw), body); err != nil {
		return err
	}
	if err := backup(c.Path, raw); err != nil {
		return err
	}
	if err := write(c.Path, body); err != nil {
		return err
	}
	return os.Remove(receiptPath(state, c.Name))
}

// applyEdits splices in order. A replacement with no offset must match exactly once: twice
// means the address is ambiguous and either choice could be the wrong one. An offset says the
// plan already resolved which occurrence it meant, so the text there is verified rather than
// counted: a container name repeated elsewhere in the document is then not ambiguity at all.
func applyEdits(body string, edits []edit) (string, error) {
	for _, e := range edits {
		if e.From == "" {
			at := e.At
			if at == atEnd {
				at = len(body)
			}
			if at < 0 || at > len(body) {
				return "", fmt.Errorf("%s: insertion point %d is outside the file", e.Address, e.At)
			}
			body = body[:at] + e.To + body[at:]
			continue
		}
		if e.At > 0 {
			if e.At+len(e.From) > len(body) || body[e.At:e.At+len(e.From)] != e.From {
				return "", fmt.Errorf("%s: the text at offset %d is no longer what the plan resolved", e.Address, e.At)
			}
			body = body[:e.At] + e.To + body[e.At+len(e.From):]
			continue
		}
		if n := strings.Count(body, e.From); n != 1 {
			return "", fmt.Errorf("%s: found %d times, want 1", e.Address, n)
		}
		body = strings.Replace(body, e.From, e.To, 1)
	}
	return body, nil
}

// write replaces the file through a temporary in the same directory, so a client reading it
// concurrently sees either the whole old file or the whole new one. The original mode is
// preserved: these files hold credentials and one of them may already be 0600.
func write(path, body string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return atomicfile.Write(path, []byte(body), info.Mode().Perm())
}

// backup keeps a timestamped copy beside the original on every mutation. It is not the
// revert path, which works on current content, but the thing to reach for when something
// else has gone wrong.
//
// Created exclusively, and suffixed until the name is free. An install and a revert can land
// in the same millisecond, and a backup that quietly overwrote the previous backup would keep
// only the state nobody needed.
func backup(path string, body []byte) error {
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	base := path + ".mcpd-" + stamp
	for n := 0; ; n++ {
		dest := base + ".bak"
		if n > 0 {
			dest = fmt.Sprintf("%s.%d.bak", base, n)
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create backup %s: %w", dest, err)
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return fmt.Errorf("write backup %s: %w", dest, err)
		}
		return f.Close()
	}
}

func receiptPath(state, client string) string {
	return filepath.Join(state, "install", client+".json")
}

func contentHash(body []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func writeReceipt(state string, rec Receipt) error {
	path := receiptPath(state, rec.Client)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create receipt directory: %w", err)
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	return atomicfile.Write(path, append(body, '\n'), 0o600)
}

func readReceipt(state, client string) (Receipt, error) {
	raw, err := os.ReadFile(receiptPath(state, client))
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, fmt.Errorf("%w: %s", ErrNotInstalled, client)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("read receipt: %w", err)
	}
	var rec Receipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Receipt{}, fmt.Errorf("parse receipt: %w", err)
	}
	return rec, nil
}

// approvalRef matches a Codex per-tool approval table header, active or left commented by
// the prototype this change supersedes.
var approvalRef = regexp.MustCompile(`(?m)^(#TS#)?\[mcp_servers\.([^.\]]+)\.tools\.("?)([^."\]]+)("?)\]`)
