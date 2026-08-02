package install

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// jsonEditor rewires a client whose configuration is JSON. All three JSON clients differ
// only in what their MCP block is called and what an entry looks like, so they share this.
type jsonEditor struct {
	// container is the object holding one key per MCP server.
	container string
	// entry is the client-specific shape of a remote server, minus the URL.
	entry map[string]any
}

// plan replaces the whole existing container with one holding mcpd alone, and hands the text it
// replaced back for the receipt to keep.
//
// It used to rename the container aside to a key of mcpd's own inside this same file. JSON has no
// comments, so there was nowhere inert to put it, and an invented key is not inert: OpenCode
// validates its configuration against a schema and refuses to start on one it does not recognise,
// which took the client down rather than only its MCP servers. Nothing mcpd invents goes in the
// file now.
func (j jsonEditor) plan(c Client, body, endpoint string) ([]edit, []string, string, error) {
	var edits []edit
	var warnings []string
	var displaced string

	container, err := j.renderContainer(endpoint)
	if err != nil {
		return nil, nil, "", err
	}

	if at, ok := findKey(body, j.container); ok {
		existing, err := serverNames(body, j.container)
		if err != nil {
			return nil, nil, "", err
		}
		if _, taken := existing[ServerName]; taken {
			return nil, nil, "", fmt.Errorf("%q already declares a server called %q", j.container, ServerName)
		}
		if _, stashed := findKey(body, StashKey); stashed {
			return nil, nil, "", fmt.Errorf("%q is already present, so a previous install was not reverted", StashKey)
		}
		_, objEnd, err := objectSpan(body, at)
		if err != nil {
			return nil, nil, "", fmt.Errorf("%q in %s: %w", j.container, c.Path, err)
		}
		// Addressed by offset: findKey resolved the top-level key by depth, and the same
		// container name legitimately recurs deeper in the document. Claude Code keeps one per
		// project it has ever opened.
		displaced = body[at:objEnd]
		edits = append(edits, edit{
			Address: j.container,
			From:    displaced,
			To:      container,
			At:      at,
			Note: fmt.Sprintf("replace %q with mcpd alone, keeping the %d displaced server(s) in mcpd's own state",
				j.container, len(existing)),
		})
		if len(existing) > 0 {
			warnings = append(warnings,
				fmt.Sprintf("%s stops loading %s directly; those backends reach it through mcpd instead",
					c.Name, strings.Join(sorted(existing), ", ")))
		}
		return edits, warnings, displaced, nil
	}

	// No container at all, so there is nothing to displace. Inserted right after the document's
	// opening brace, which is the one position that does not depend on anything else in the file
	// being where it was.
	open := strings.Index(body, "{")
	if open < 0 {
		return nil, nil, "", fmt.Errorf("not a JSON object")
	}
	edits = append(edits, edit{
		Address: j.container + "." + ServerName,
		To:      "\n  " + container + ",",
		At:      open + 1,
		Note:    fmt.Sprintf("point %q at %s", j.container+"."+ServerName, endpoint),
	})
	return edits, warnings, "", nil
}

// revert undoes an install against the file's current bytes.
//
// It cannot be the plain textual inverse of the install, and that is the whole subtlety
// here: the install created the container, so a server the user declares afterwards lands
// inside the very text the install wrote. Matching that text would then refuse a revert for
// an edit that has nothing to do with mcpd. So the container is taken apart structurally:
// mcpd's own entry is the only thing mcpd owns, anything else found beside it is the user's
// and is carried over, and the container itself only goes away because mcpd put it there.
func (j jsonEditor) revert(c Client, body string, rec Receipt) ([]edit, error) {
	cStart, ok := findKey(body, j.container)
	if !ok {
		return nil, fmt.Errorf("%w: %q is gone from %s", ErrConflict, j.container, c.Path)
	}
	objStart, objEnd, err := objectSpan(body, cStart)
	if err != nil {
		return nil, fmt.Errorf("%w: %q in %s: %v", ErrConflict, j.container, c.Path, err)
	}
	found, err := members(body, objStart)
	if err != nil {
		return nil, fmt.Errorf("%w: %q in %s: %v", ErrConflict, j.container, c.Path, err)
	}

	var mine string
	var theirs []string
	for _, m := range found {
		if m.name == ServerName {
			mine = body[m.start:m.end]
			continue
		}
		theirs = append(theirs, body[m.start:m.end])
	}
	if mine == "" {
		return nil, fmt.Errorf("%w: %s.%s is gone from %s", ErrConflict, j.container, ServerName, c.Path)
	}
	want, err := j.canonical(rec.Endpoint)
	if err != nil {
		return nil, err
	}
	got, err := canonicalMember(mine)
	if err != nil || got != want {
		return nil, fmt.Errorf("%w: %s.%s in %s", ErrConflict, j.container, ServerName, c.Path)
	}

	// A receipt written before the displaced text moved out of the file: the stash is still in
	// there under mcpd's own key, so it is put back the way it was put aside. Kept because
	// receipts written by that scheme exist on disk, and a revert that cannot read them would
	// strand the very declarations it is meant to restore.
	if rec.Displaced == "" {
		if _, stashed := findKey(body, StashKey); stashed {
			return j.revertInFileStash(body, cStart, objEnd, theirs)
		}
		// Nothing was displaced, so the container is mcpd's own and goes away with it.
		from := lineStart(body, cStart)
		to := objEnd
		if to < len(body) && body[to] == ',' {
			to++
		}
		return []edit{{
			Address: j.container + "." + ServerName,
			From:    body[from:to],
			To:      "",
			At:      from,
			Note:    "remove " + j.container + "." + ServerName,
		}}, nil
	}

	// The displaced text goes back verbatim, with anything the user declared alongside mcpd
	// since installing folded into it. A snapshot restore would silently destroy those, which
	// is why revert works on current content rather than on the bytes the install saw.
	restored, err := mergeIntoContainer(rec.Displaced, theirs)
	if err != nil {
		return nil, fmt.Errorf("%w: %q in %s: %v", ErrConflict, j.container, c.Path, err)
	}
	note := fmt.Sprintf("put the displaced server(s) back under %q", j.container)
	if len(theirs) > 0 {
		note += fmt.Sprintf(", carrying over the %d declared after installing", len(theirs))
	}
	return []edit{{
		Address: j.container,
		From:    body[cStart:objEnd],
		To:      restored,
		At:      cStart,
		Note:    note,
	}}, nil
}

// mergeIntoContainer folds members the user added into the displaced container text, which is a
// `"name": { ... }` fragment exactly as it was lifted out of the file.
func mergeIntoContainer(displaced string, theirs []string) (string, error) {
	if len(theirs) == 0 {
		return displaced, nil
	}
	objStart, _, err := objectSpan(displaced, 0)
	if err != nil {
		return "", err
	}
	existing, err := members(displaced, objStart)
	if err != nil {
		return "", err
	}
	added := "\n    " + strings.Join(theirs, ",\n    ")
	if len(existing) > 0 {
		added += ","
	}
	return displaced[:objStart+1] + added + displaced[objStart+1:], nil
}

// revertInFileStash restores an install that renamed the container aside inside the file itself.
func (j jsonEditor) revertInFileStash(body string, cStart, objEnd int, theirs []string) ([]edit, error) {
	from := lineStart(body, cStart)
	to := objEnd
	if to < len(body) && body[to] == ',' {
		to++
	}
	edits := []edit{{
		Address: j.container + "." + ServerName,
		From:    body[from:to],
		To:      "",
		Note:    "remove " + j.container + "." + ServerName,
	}}
	unstashed, err := j.unstash(body, theirs)
	if err != nil {
		return nil, err
	}
	return append(edits, unstashed), nil
}

// unstash renames the stash back to the container and, in the same edit, carries over any
// server the user declared after installing. A snapshot restore would silently destroy those,
// which is why revert works on current content.
//
// It is one edit and not two because the stash key is the only text here that identifies this
// spot unambiguously. Anchoring the carry-over on the container name instead is ambiguous in a
// document that repeats it, and renaming first would consume the one unique anchor before the
// carry-over could use it.
func (j jsonEditor) unstash(body string, theirs []string) (edit, error) {
	at, ok := findKey(body, StashKey)
	if !ok {
		return edit{}, fmt.Errorf("%w: %q is gone, so there is nothing to put back", ErrConflict, StashKey)
	}
	objStart, _, err := objectSpan(body, at)
	if err != nil {
		return edit{}, fmt.Errorf("%w: %q: %v", ErrConflict, StashKey, err)
	}
	existing, err := members(body, objStart)
	if err != nil {
		return edit{}, fmt.Errorf("%w: %q: %v", ErrConflict, StashKey, err)
	}
	// Spans the key's opening quote through its opening brace, so whatever the user had
	// between the two survives and the rename stays a rename.
	note := fmt.Sprintf("put the servers in %q back under %q", StashKey, j.container)
	to := `"` + j.container + `"` + body[at+len(StashKey)+2:objStart+1]
	if len(theirs) > 0 {
		added := "\n    " + strings.Join(theirs, ",\n    ")
		if len(existing) > 0 {
			added += ","
		}
		to += added
		note += fmt.Sprintf(", carrying over the %d declared after installing", len(theirs))
	}
	return edit{Address: StashKey, From: body[at : objStart+1], To: to, Note: note}, nil
}

// validate reports whether the result is still JSON the client can read.
func (jsonEditor) validate(body string) error {
	return json.Unmarshal([]byte(body), new(any))
}

// canonical is mcpd's entry as the client will read it, with no layout, for comparing
// against what is in the file now.
func (j jsonEditor) canonical(endpoint string) (string, error) {
	raw, err := json.Marshal(j.merged(endpoint))
	if err != nil {
		return "", fmt.Errorf("encode entry: %w", err)
	}
	return string(raw), nil
}

func (j jsonEditor) merged(endpoint string) map[string]any {
	entry := map[string]any{"url": endpoint}
	for k, v := range j.entry {
		entry[k] = v
	}
	return entry
}

// canonicalMember re-encodes `"name": value` as its value alone, so a comparison is about
// what the client will read rather than how it happens to be laid out. Reindenting mcpd's
// entry is not a conflict; changing its URL is.
func canonicalMember(member string) (string, error) {
	_, value, found := strings.Cut(member, ":")
	if !found {
		return "", fmt.Errorf("not a member")
	}
	var v any
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		return "", err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// renderContainer builds the container holding mcpd and nothing else, on its own lines so a
// reader can see at a glance what mcpd put there. It carries no leading newline and no trailing
// comma, because it replaces an existing `"name": {...}` fragment in place; the insertion path
// adds both when there was no container to replace.
func (j jsonEditor) renderContainer(endpoint string) (string, error) {
	inner, err := json.MarshalIndent(j.merged(endpoint), "    ", "  ")
	if err != nil {
		return "", fmt.Errorf("encode entry: %w", err)
	}
	return fmt.Sprintf("%q: {\n    %q: %s\n  }", j.container, ServerName, string(inner)), nil
}

// findKey reports where a top-level object key starts, including its opening quote. Only the
// top level is searched, because that is the only place these containers live and a nested
// key of the same name belongs to something else.
func findKey(body, key string) (int, bool) {
	needle := `"` + key + `"`
	depth := 0
	inString, escaped := false, false
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			if depth == 1 && strings.HasPrefix(body[i:], needle) {
				return i, true
			}
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return 0, false
}

// objectSpan returns the span of the object value belonging to the key starting at keyStart:
// the offset of its opening brace, and the offset just past its closing brace.
func objectSpan(body string, keyStart int) (start, end int, err error) {
	i, err := skipString(body, keyStart)
	if err != nil {
		return 0, 0, err
	}
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r' || body[i] == ':') {
		i++
	}
	if i >= len(body) || body[i] != '{' {
		return 0, 0, fmt.Errorf("value is not an object")
	}
	end, err = skipValue(body, i)
	return i, end, err
}

type member struct {
	name  string
	start int // the member's opening quote
	end   int // just past its value
}

// members lists the entries of the object whose opening brace is at objStart, with the byte
// span of each, so one can be cut out and the rest carried over verbatim.
func members(body string, objStart int) ([]member, error) {
	i := objStart + 1
	var out []member
	for {
		for i < len(body) && isSpace(body[i]) {
			i++
		}
		if i >= len(body) {
			return nil, fmt.Errorf("unterminated object")
		}
		if body[i] == '}' {
			return out, nil
		}
		if body[i] == ',' {
			i++
			continue
		}
		if body[i] != '"' {
			return nil, fmt.Errorf("unexpected %q where a key was expected", body[i])
		}
		keyStart := i
		afterKey, err := skipString(body, i)
		if err != nil {
			return nil, err
		}
		var name string
		if err := json.Unmarshal([]byte(body[keyStart:afterKey]), &name); err != nil {
			return nil, err
		}
		i = afterKey
		for i < len(body) && isSpace(body[i]) {
			i++
		}
		if i >= len(body) || body[i] != ':' {
			return nil, fmt.Errorf("missing colon after %q", name)
		}
		i++
		for i < len(body) && isSpace(body[i]) {
			i++
		}
		end, err := skipValue(body, i)
		if err != nil {
			return nil, err
		}
		out = append(out, member{name: name, start: keyStart, end: end})
		i = end
	}
}

// skipValue returns the offset just past the JSON value starting at i.
func skipValue(body string, i int) (int, error) {
	if i >= len(body) {
		return 0, fmt.Errorf("value expected")
	}
	switch body[i] {
	case '"':
		return skipString(body, i)
	case '{', '[':
		depth := 0
		for ; i < len(body); i++ {
			switch body[i] {
			case '"':
				next, err := skipString(body, i)
				if err != nil {
					return 0, err
				}
				i = next - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, fmt.Errorf("unterminated object or array")
	default:
		for ; i < len(body); i++ {
			if isSpace(body[i]) || body[i] == ',' || body[i] == '}' || body[i] == ']' {
				return i, nil
			}
		}
		return i, nil
	}
}

func skipString(body string, i int) (int, error) {
	if i >= len(body) || body[i] != '"' {
		return 0, fmt.Errorf("string expected")
	}
	for i++; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated string")
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// lineStart walks back to the beginning of the line holding at, so removing a region takes
// the indent that was inserted with it.
func lineStart(body string, at int) int {
	for i := at - 1; i >= 0; i-- {
		if body[i] == '\n' {
			return i
		}
		if body[i] != ' ' && body[i] != '\t' {
			return at
		}
	}
	return at
}

// serverNames reads the keys already declared in the container, for the plan's own reporting
// and to refuse an install that would collide with one of them.
func serverNames(body, container string) (map[string]struct{}, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	raw, ok := doc[container]
	if !ok {
		return map[string]struct{}{}, nil
	}
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("parse %q: %w", container, err)
	}
	out := make(map[string]struct{}, len(block))
	for name := range block {
		out[name] = struct{}{}
	}
	return out, nil
}

func sorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
