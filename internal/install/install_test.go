package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden inputs are cut down from the real files on this machine, keeping the shapes
// that matter: a hand-chosen key order, an unrelated setting the tool must not touch, a
// credential reference, and for Codex the per-tool approval tables the prototype left
// commented out.
const claudeGolden = `{
  "numStartups": 412,
  "mcpServers": {
    "notion": {
      "type": "http",
      "url": "https://mcp.notion.com/mcp"
    }
  },
  "hasSeenTasksHint": true
}
`

const cursorGolden = `{
  "mcpServers": {
    "datadog-mcp": {
      "type": "http",
      "url": "https://ai.example.test/mcp/datadog",
      "headers": { "Authorization": "Bearer ${LITELLM_KEY}" }
    }
  }
}
`

const opencodeGolden = `{
  "$schema": "https://opencode.ai/config.json",
  "provider": { "anthropic": { "options": {} } },
  "mcp": {
    "github": {
      "type": "remote",
      "url": "https://ai.example.test/mcp/github",
      "timeout": 120
    }
  }
}
`

const codexGolden = `model = "gpt-5.6-terra"

[mcp_servers.notion]
url = "https://mcp.notion.com/mcp"

#TS#[mcp_servers.github]
#TS#url = "https://ai.example.test/mcp/github"
#TS#bearer_token_env_var = "LITELLM_KEY"
#TS#
#TS#[mcp_servers.github.tools.create_pull_request]
#TS#approval_mode = "approve"
#TS#
#TS#[mcp_servers.articulate_knowledge]
#TS#url = "https://ai.example.test/mcp/ak"
#TS#
#TS#[mcp_servers.articulate_knowledge.tools.articulate_knowledge-search_articulate_knowledge]
#TS#approval_mode = "approve"
#TS#
#TS#[mcp_servers.articulate_knowledge.tools.search_articulate_knowledge]
#TS#approval_mode = "approve"

[mcp_servers.tool-search]
command = "uvx"

[mcp_servers.tool-search.env]
LITELLM_KEY = "${LITELLM_KEY}"

[projects."/home/alex"]
trust_level = "trusted"
`

type fixture struct {
	home   string
	state  string
	client Client
	body   string
}

func newFixture(t *testing.T, name, body string) *fixture {
	t.Helper()
	home := t.TempDir()
	c, err := Lookup(home, name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(c.Path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return &fixture{home: home, state: filepath.Join(home, "state"), client: c, body: body}
}

func (f *fixture) install(t *testing.T) {
	t.Helper()
	p, err := f.client.PlanInstall("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if err := f.client.Apply(f.state, p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func (f *fixture) revert(t *testing.T) {
	t.Helper()
	p, err := f.client.PlanRevert(f.state)
	if err != nil {
		t.Fatalf("PlanRevert: %v", err)
	}
	if err := f.client.Revert(f.state, p); err != nil {
		t.Fatalf("Revert: %v", err)
	}
}

func (f *fixture) read(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(f.client.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(raw)
}

func golden(name string) string {
	switch name {
	case "claude":
		return claudeGolden
	case "cursor":
		return cursorGolden
	case "opencode":
		return opencodeGolden
	default:
		return codexGolden
	}
}

// Scenario: "With no intervening edits, revert is exact." This is what forces every edit to
// be a text splice: a parse and re-serialize would reformat the file and this could never
// hold, whatever else the code did.
func TestRevertWithNoInterveningEditsIsByteForByte(t *testing.T) {
	for _, name := range []string{"claude", "codex", "cursor", "opencode"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, name, golden(name))
			f.install(t)
			if after := f.read(t); after == f.body {
				t.Fatal("install changed nothing, so this test proves nothing about revert")
			}
			f.revert(t)
			if got := f.read(t); got != f.body {
				t.Errorf("revert is not byte-for-byte:\n--- got ---\n%s\n--- want ---\n%s", got, f.body)
			}
		})
	}
}

// Scenario: "A client is rewired to the correct endpoint." Pass-through for the two clients
// with native tool search, the facade for the two that would otherwise load every schema.
func TestEachClientIsPointedAtTheEndpointItsCapabilitiesCallFor(t *testing.T) {
	for name, want := range map[string]string{
		"claude":   "http://127.0.0.1:7420/mcp/passthrough",
		"codex":    "http://127.0.0.1:7420/mcp/passthrough",
		"cursor":   "http://127.0.0.1:7420/mcp/search",
		"opencode": "http://127.0.0.1:7420/mcp/search",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, name, golden(name))
			f.install(t)
			body := f.read(t)
			if !strings.Contains(body, want) {
				t.Errorf("%s is not pointed at %s:\n%s", name, want, body)
			}
			if other := strings.Replace(want, string(f.client.Mode), otherMode(f.client.Mode), 1); strings.Contains(body, other) {
				t.Errorf("%s is pointed at both endpoints", name)
			}
		})
	}
}

func otherMode(m Mode) string {
	if m == Passthrough {
		return string(Search)
	}
	return string(Passthrough)
}

// A JSON client ends up declaring mcpd and nothing else, with everything it used to declare
// stashed rather than deleted. Asserted by parsing, because what matters is what the client
// will read, not what the bytes look like.
func TestAJSONClientDeclaresOnlyMcpdAndKeepsWhatItHad(t *testing.T) {
	for _, tc := range []struct{ name, container string }{
		{"claude", "mcpServers"}, {"cursor", "mcpServers"}, {"opencode", "mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.name, golden(tc.name))
			before := parse(t, f.body)
			f.install(t)
			after := parse(t, f.read(t))

			block, ok := after[tc.container].(map[string]any)
			if !ok {
				t.Fatalf("%q is missing or not an object after install", tc.container)
			}
			if len(block) != 1 {
				t.Errorf("%q declares %d servers, want only mcpd: %v", tc.container, len(block), keysOf(block))
			}
			if _, ok := block[ServerName]; !ok {
				t.Errorf("%q does not declare %q", tc.container, ServerName)
			}
			stash, ok := after[StashKey].(map[string]any)
			if !ok {
				t.Fatalf("%q is missing, so the user's declarations were deleted rather than stashed", StashKey)
			}
			original := before[tc.container].(map[string]any)
			if len(stash) != len(original) {
				t.Errorf("stash holds %d of %d declarations", len(stash), len(original))
			}
			for name, want := range original {
				if got := stash[name]; !sameJSON(t, got, want) {
					t.Errorf("stashed %s changed:\n got %v\nwant %v", name, got, want)
				}
			}
			// Everything the tool has no business touching.
			for key := range before {
				if key == tc.container {
					continue
				}
				if !sameJSON(t, after[key], before[key]) {
					t.Errorf("install changed the unrelated key %q", key)
				}
			}
		})
	}
}

// Scenario: "An approval gate survives rewiring", and the guardrail that gives this
// capability its whole reason for existing: the gate must be active, not commented out.
func TestCodexApprovalGatesSurviveUnderTheNewToolNames(t *testing.T) {
	f := newFixture(t, "codex", codexGolden)
	f.install(t)
	body := f.read(t)

	for _, want := range []string{
		`[mcp_servers.mcpd.tools."mcp__github__create_pull_request"]`,
		`[mcp_servers.mcpd.tools."mcp__articulate_knowledge__search_articulate_knowledge"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the migrated config has no %s:\n%s", want, body)
		}
		// Active, and on its own line: a migrated gate that arrives commented out is the
		// silent loss this requirement exists to prevent.
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, want) && strings.HasPrefix(strings.TrimSpace(line), "#") {
				t.Errorf("%s was migrated commented out: %q", want, line)
			}
		}
	}
	if n := strings.Count(body, `approval_mode = "approve"`) - strings.Count(body, `#TS#approval_mode = "approve"`); n != 2 {
		t.Errorf("active approval_mode settings = %d, want 2:\n%s", n, body)
	}
	// The prototype already prefixed one tool with its server name. Prefixing it again
	// would produce a key that matches no tool the daemon serves, and would fail silently.
	if strings.Contains(body, "mcp__articulate_knowledge__articulate_knowledge-") {
		t.Error("an already-prefixed tool name was prefixed a second time")
	}
	// The real file declares that one tool twice, prefixed and unprefixed, and both reduce to
	// one id. Two tables of the same name is a duplicate TOML key, and Codex then loads no
	// servers at all: the migration written to protect one gate would remove every one.
	const once = `[mcp_servers.mcpd.tools."mcp__articulate_knowledge__search_articulate_knowledge"]`
	if n := strings.Count(body, once); n != 1 {
		t.Errorf("the migrated table appears %d times, want 1: duplicate keys make config.toml unloadable", n)
	}
	// A sub-table of a stashed server has to move with its parent. Left behind, it recreates
	// its parent as a server with no command and no url, which is a broken declaration.
	if strings.Contains(body, "[mcp_servers.tool-search") {
		t.Errorf("a stashed server left a sub-table behind under mcp_servers:\n%s", body)
	}
}

// Scenario: "An unrelated later edit survives revert." A snapshot restore would destroy it,
// which is why revert works on current content instead.
func TestAnUnrelatedLaterEditSurvivesRevert(t *testing.T) {
	f := newFixture(t, "cursor", cursorGolden)
	f.install(t)

	// The user adds a server of their own after installing.
	body := f.read(t)
	const mine = `"mcpServers": {`
	body = strings.Replace(body, mine, mine+"\n    \"mine\": { \"type\": \"http\", \"url\": \"https://mine.test/mcp\" },", 1)
	if err := os.WriteFile(f.client.Path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f.revert(t)

	after := parse(t, f.read(t))
	block := after["mcpServers"].(map[string]any)
	if _, ok := block["mine"]; !ok {
		t.Error("revert destroyed a server the user added after installing")
	}
	if _, ok := block[ServerName]; ok {
		t.Error("revert left mcpd behind")
	}
	if _, ok := block["datadog-mcp"]; !ok {
		t.Error("revert did not put the stashed declarations back")
	}
	if _, ok := after[StashKey]; ok {
		t.Error("revert left the stash key behind")
	}
}

// Scenario: "A hand-modified owned region blocks revert", naming the file and the key.
// Guessing whose version wins is the one thing worse than refusing.
func TestAHandEditedOwnedRegionBlocksRevertAndNamesIt(t *testing.T) {
	f := newFixture(t, "cursor", cursorGolden)
	f.install(t)

	body := strings.Replace(f.read(t), "/mcp/search", "/mcp/passthrough", 1)
	if err := os.WriteFile(f.client.Path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := f.client.PlanRevert(f.state)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("PlanRevert = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), f.client.Path) {
		t.Errorf("the refusal does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "mcpServers."+ServerName) {
		t.Errorf("the refusal does not name the key: %v", err)
	}
	if got := f.read(t); got != body {
		t.Error("a refused revert still changed the file")
	}
}

// Scenario: "Changes can be previewed before being applied."
func TestAPlanChangesNothingAndSaysWhatItWouldDo(t *testing.T) {
	f := newFixture(t, "codex", codexGolden)

	p, err := f.client.PlanInstall("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if got := f.read(t); got != f.body {
		t.Error("planning changed the file")
	}
	if p.Empty() {
		t.Fatal("the plan is empty")
	}
	joined := strings.Join(p.Notes, "\n")
	for _, want := range []string{"mcp/passthrough", "[mcp_servers.notion]", "create_pull_request"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(receiptPath(f.state, "codex")); err == nil {
		t.Error("planning wrote a receipt")
	}
}

// Every mutation leaves a timestamped copy beside the original. It is not the revert path,
// which works on current content, but the thing to reach for when something else went wrong.
func TestEveryMutationLeavesATimestampedBackup(t *testing.T) {
	f := newFixture(t, "opencode", opencodeGolden)
	f.install(t)
	f.revert(t)

	entries, err := os.ReadDir(filepath.Dir(f.client.Path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var backups []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mcpd-") && strings.HasSuffix(e.Name(), ".bak") {
			backups = append(backups, e.Name())
		}
	}
	// Two mutations, so two backups: an install and a revert can land in the same
	// millisecond, and one silently overwriting the other keeps only the state nobody needs.
	if len(backups) != 2 {
		t.Fatalf("backups = %v, want one per mutation", backups)
	}
	held := false
	for _, name := range backups {
		full := filepath.Join(filepath.Dir(f.client.Path), name)
		raw, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(raw) == f.body {
			held = true
		}
		// A file holding credentials, so the copy of it is no more readable.
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
	if !held {
		t.Error("no backup holds the content from before the install")
	}
}

// Installing twice is refused rather than applied twice, because the second install would
// stash mcpd's own declaration and leave a revert with nothing coherent to undo.
func TestASecondInstallIsRefused(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, name, golden(name))
			f.install(t)
			if _, err := f.client.PlanInstall("127.0.0.1:7420"); err == nil {
				t.Error("a second install was planned without complaint")
			}
		})
	}
}

func TestRevertingSomethingNeverInstalledSaysSo(t *testing.T) {
	f := newFixture(t, "cursor", cursorGolden)
	if _, err := f.client.PlanRevert(f.state); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("PlanRevert = %v, want ErrNotInstalled", err)
	}
}

func parse(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	return out
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(left) == string(right)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
