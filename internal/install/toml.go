package install

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ahodges22/mcpd/internal/catalog"
)

// tomlEditor rewires Codex.
type tomlEditor struct{}

// commentPrefix marks a line this tool made inert. TOML has comments, so the displaced tables can
// stay exactly where the user wrote them rather than moving into mcpd's state: a comment cannot be
// rejected by a client, which is the property that matters, and leaving the text in place keeps it
// readable and keeps revert a pure text inverse.
//
// This replaces renaming each table into `[_mcpd_stashed.*]`. That was a key mcpd invented, and a
// client is entitled to reject one: OpenCode refuses to start on an unrecognised key in its own
// format. Codex tolerated it, but tolerance is the client's choice to withdraw, so it was the same
// defect waiting for a version bump.
const commentPrefix = "#mcpd# "

func (tomlEditor) plan(c Client, body, endpoint string) ([]edit, []string, string, error) {
	var edits []edit
	var warnings []string

	if strings.Contains(body, "[mcp_servers."+ServerName+"]") {
		return nil, nil, "", fmt.Errorf("config.toml already declares [mcp_servers.%s]", ServerName)
	}
	if strings.Contains(body, "["+StashKey+".") {
		return nil, nil, "", fmt.Errorf("[%s.*] is already present, so a previous install was not reverted", StashKey)
	}
	if strings.Contains(body, commentPrefix) {
		return nil, nil, "", fmt.Errorf("%q is already present, so a previous install was not reverted", strings.TrimSpace(commentPrefix))
	}

	// Each run of server tables is commented out whole, in one edit, so the bodies keep their
	// bytes and a revert is the same edit inverted. Addressed by content rather than by offset:
	// a run contains its own table header, TOML forbids declaring the same table twice, so the
	// text is unique and no offset has to survive the edits applied before it.
	blocks, err := serverBlocks(body)
	if err != nil {
		return nil, nil, "", err
	}
	for _, b := range blocks {
		edits = append(edits, edit{
			Address: b.address,
			From:    b.text,
			To:      commentOut(b.text),
			Note:    "comment out [" + b.address + "]",
		})
	}
	if n := len(blocks); n > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"codex stops loading %d server table run(s) directly; those backends reach it through mcpd instead", n))
	}

	approvals := migrateApprovals(body)
	entry := "\n[mcp_servers." + ServerName + "]\nurl = \"" + endpoint + "\"\n" + approvals.text
	edits = append(edits, edit{
		Address: "mcp_servers." + ServerName,
		To:      entry,
		At:      atEnd,
		Note:    "point [mcp_servers." + ServerName + "] at " + endpoint,
	})
	for _, note := range approvals.notes {
		edits[len(edits)-1].Note += "\n      " + note
	}
	// Nothing leaves the file, so the receipt holds no displaced text.
	return edits, warnings, "", nil
}

type serverBlock struct {
	address string
	text    string
}

// serverBlocks returns each contiguous run of `[mcp_servers.*]` tables, with the run's own text.
// A table runs from its header to the next line that opens a different table, or to the end of the
// file, so a run carries its keys, its sub-tables and the blank lines between them.
func serverBlocks(body string) ([]serverBlock, error) {
	lines := strings.SplitAfter(body, "\n")
	inside := make([]bool, len(lines))
	within := false
	var multiline string
	for i, line := range lines {
		if multiline == "" && strings.HasPrefix(line, "[") {
			within = strings.HasPrefix(line, "[mcp_servers.")
		}
		inside[i] = within
		advanceMultiline(line, &multiline)
	}

	var out []serverBlock
	var run strings.Builder
	var address string
	flush := func() {
		if run.Len() == 0 {
			return
		}
		out = append(out, serverBlock{address: address, text: run.String()})
		run.Reset()
		address = ""
	}
	for i, line := range lines {
		if !inside[i] {
			flush()
			continue
		}
		if address == "" {
			end := strings.Index(line, "]")
			if end < 0 {
				return nil, fmt.Errorf("unterminated table header: %q", strings.TrimSpace(line))
			}
			address = strings.Trim(line[:end+1], "[]")
		}
		run.WriteString(line)
	}
	flush()
	return out, nil
}

func advanceMultiline(line string, delimiter *string) {
	if *delimiter != "" {
		if tripleQuote(line, *delimiter, 0) >= 0 {
			*delimiter = ""
		}
		return
	}
	var quote byte
	for i := 0; i < len(line); i++ {
		if quote != 0 {
			if quote == '"' && line[i] == '\\' {
				i++
				continue
			}
			if line[i] == quote {
				quote = 0
			}
			continue
		}
		if line[i] == '#' {
			return
		}
		candidate := ""
		switch {
		case strings.HasPrefix(line[i:], `"""`):
			candidate = `"""`
		case strings.HasPrefix(line[i:], `'''`):
			candidate = `'''`
		}
		if candidate != "" {
			if tripleQuote(line, candidate, i+len(candidate)) < 0 {
				*delimiter = candidate
			}
			return
		}
		if line[i] == '"' || line[i] == '\'' {
			quote = line[i]
		}
	}
}

func tripleQuote(line, delimiter string, start int) int {
	for i := start; i+len(delimiter) <= len(line); i++ {
		if !strings.HasPrefix(line[i:], delimiter) {
			continue
		}
		if delimiter == `"""` {
			backslashes := 0
			for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 != 0 {
				continue
			}
		}
		return i
	}
	return -1
}

// commentOut makes every line of a run inert, preserving the bytes after the prefix so stripping it
// again restores the run exactly.
func commentOut(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		b.WriteString(commentPrefix)
		b.WriteString(line)
	}
	return b.String()
}

type approvalSet struct {
	text  string
	notes []string
}

// migrateApprovals rewrites every per-tool approval Codex declares onto the names the tool
// has through pass-through. This is the highest-consequence thing in this package and it
// fails silently: a dropped or mistyped key leaves the file valid and the daemon working
// while a destructive tool loses its approval gate, and the only symptom is the absence of a
// prompt the user was not expecting.
//
// Tables the superseded prototype left commented out are migrated too, and migrated active.
// They are exactly the gates this migration exists to rescue: leaving them commented is the
// same silent loss by another route.
func migrateApprovals(body string) approvalSet {
	var out approvalSet
	// Two of the real declarations name the same tool, one already carrying its server prefix
	// and one not, and both canonicalize to a single id. Emitting that table twice is a
	// duplicate TOML key, which Codex refuses to load at all, so the migration meant to
	// protect one gate would take out every server in the file.
	seen := make(map[string]struct{})
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		m := approvalRef.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		server, tool := m[2], m[4]
		// The upstream name, not the name the client used: a tool the prototype already
		// renamed carries its server prefix, and prefixing it again would name nothing.
		tool = strings.TrimPrefix(tool, server+"-")
		id := catalog.CanonicalID(server, tool)
		mode, ok := approvalMode(lines[i+1:], m[1] != "")
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			out.notes = append(out.notes,
				fmt.Sprintf("%s is declared more than once; the first gate is kept", id))
			continue
		}
		seen[id] = struct{}{}
		out.text += fmt.Sprintf("\n[mcp_servers.%s.tools.%q]\napproval_mode = %q\n", ServerName, id, mode)
		note := fmt.Sprintf("carry over approval_mode = %q for %s", mode, id)
		if m[1] != "" {
			note += " (it was commented out, and is restored active)"
		}
		out.notes = append(out.notes, note)
	}
	return out
}

// approvalMode reads the approval_mode belonging to the header just passed, stopping at the
// next table so a table with no approval_mode does not inherit the next one's.
func approvalMode(rest []string, commented bool) (string, bool) {
	for _, line := range rest {
		trimmed := strings.TrimPrefix(line, "#TS#")
		if commented && !strings.HasPrefix(line, "#TS#") && strings.TrimSpace(line) != "" {
			return "", false
		}
		trimmed = strings.TrimSpace(trimmed)
		if strings.HasPrefix(trimmed, "[") {
			return "", false
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(key) != "approval_mode" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`), true
	}
	return "", false
}

// validate reports whether the result is still TOML Codex can read. A duplicate key counts:
// TOML forbids redefining a table, so a migration that emitted one would take the whole file
// down rather than only its own table.
func (tomlEditor) validate(body string) error {
	_, err := toml.Decode(body, new(map[string]any))
	return err
}

// revert is the plain textual inverse. It is enough here, unlike for JSON: mcpd appends its
// own table at the end of the file and renames existing headers in place, so a table the user
// adds afterwards is its own lines and touches neither.
func (tomlEditor) revert(c Client, body string, rec Receipt) ([]edit, error) {
	return textualInverse(c, body, rec)
}
