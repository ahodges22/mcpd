package install

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ahodges/mcpd/internal/catalog"
)

// serverHeader matches a Codex MCP server table header at the start of a line. Anchored
// there so a header the prototype commented out, which begins with its own marker, is left
// alone: it is already inert, and the only thing wanted from it is its approvals.
var serverHeader = regexp.MustCompile(`(?m)^\[mcp_servers\.`)

// tomlEditor rewires Codex.
type tomlEditor struct{}

func (tomlEditor) plan(c Client, body, endpoint string) ([]edit, []string, error) {
	var edits []edit
	var warnings []string

	if strings.Contains(body, "[mcp_servers."+ServerName+"]") {
		return nil, nil, fmt.Errorf("config.toml already declares [mcp_servers.%s]", ServerName)
	}
	if strings.Contains(body, "["+StashKey+".") {
		return nil, nil, fmt.Errorf("[%s.*] is already present, so a previous install was not reverted", StashKey)
	}

	// Every active server table is renamed into an unknown top-level table, which Codex
	// ignores. One replacement per header, so the bodies keep their bytes and a revert is
	// the same rename in the other direction.
	for _, loc := range serverHeader.FindAllStringIndex(body, -1) {
		close := strings.Index(body[loc[0]:], "]")
		if close < 0 {
			return nil, nil, fmt.Errorf("unterminated table header at offset %d", loc[0])
		}
		full := body[loc[0] : loc[0]+close+1]
		edits = append(edits, edit{
			Address: strings.Trim(full, "[]"),
			From:    full,
			To:      "[" + StashKey + "." + strings.TrimPrefix(full, "[mcp_servers."),
			Note:    "move " + full + " aside",
		})
	}
	if n := len(edits); n > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"codex stops loading %d server table(s) directly; those backends reach it through mcpd instead", n))
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
	return edits, warnings, nil
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
