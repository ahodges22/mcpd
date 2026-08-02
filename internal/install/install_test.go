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
      "headers": { "Authorization": "Bearer ${GATEWAY_TOKEN}" }
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
#TS#bearer_token_env_var = "GATEWAY_TOKEN"
#TS#
#TS#[mcp_servers.github.tools.create_pull_request]
#TS#approval_mode = "approve"
#TS#
#TS#[mcp_servers.knowledge_base]
#TS#url = "https://ai.example.test/mcp/ak"
#TS#
#TS#[mcp_servers.knowledge_base.tools.knowledge_base-search]
#TS#approval_mode = "approve"
#TS#
#TS#[mcp_servers.knowledge_base.tools.search]
#TS#approval_mode = "approve"

[mcp_servers.tool-search]
command = "uvx"

[mcp_servers.tool-search.env]
GATEWAY_TOKEN = "${GATEWAY_TOKEN}"

[projects."/home/user"]
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
			// Nothing mcpd invented is written into the file. A client is entitled to reject a
			// key it does not know, and OpenCode does: it validates against a schema and refuses
			// to start, so the displaced declarations live in mcpd's own state instead.
			for key := range after {
				if strings.HasPrefix(key, "_mcpd") {
					t.Errorf("install wrote %q into the client's own file", key)
				}
			}
			// Displaced rather than deleted, which for this scheme means revert can produce them
			// again. That is the only place the guarantee is observable now.
			original := before[tc.container].(map[string]any)
			f.revert(t)
			restored, ok := parse(t, f.read(t))[tc.container].(map[string]any)
			if !ok {
				t.Fatalf("%q is missing after revert, so the displaced declarations were lost", tc.container)
			}
			if len(restored) != len(original) {
				t.Errorf("revert restored %d of %d declarations: %v", len(restored), len(original), keysOf(restored))
			}
			for name, want := range original {
				if got := restored[name]; !sameJSON(t, got, want) {
					t.Errorf("restored %s changed:\n got %v\nwant %v", name, got, want)
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
		`[mcp_servers.mcpd.tools."mcp__knowledge_base__search"]`,
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
	if strings.Contains(body, "mcp__knowledge_base__knowledge_base-") {
		t.Error("an already-prefixed tool name was prefixed a second time")
	}
	// The real file declares that one tool twice, prefixed and unprefixed, and both reduce to
	// one id. Two tables of the same name is a duplicate TOML key, and Codex then loads no
	// servers at all: the migration written to protect one gate would remove every one.
	const once = `[mcp_servers.mcpd.tools."mcp__knowledge_base__search"]`
	if n := strings.Count(body, once); n != 1 {
		t.Errorf("the migrated table appears %d times, want 1: duplicate keys make config.toml unloadable", n)
	}
	// A sub-table of a displaced server has to go inert with its parent. Left live, it recreates
	// its parent as a server with no command and no url, which is a broken declaration. Checked
	// per line rather than by substring, because a commented line still contains its own text.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, commentPrefix) {
			continue
		}
		if strings.HasPrefix(line, "[mcp_servers.") && !strings.HasPrefix(line, "[mcp_servers."+ServerName) {
			t.Errorf("a displaced server left a live table behind: %q\n%s", line, body)
		}
	}
	// And nothing mcpd invented is a key: the displaced tables are comments, which no client can
	// reject, rather than a table under a name of mcpd's own.
	if strings.Contains(body, "["+StashKey+".") {
		t.Errorf("install wrote a %q table into the client's own file:\n%s", StashKey, body)
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

// A real ~/.claude.json carries one "mcpServers" key per project it has ever opened, so the
// container name occurs many times over. Addressing an edit by unique text occurrence refused
// the install outright on every such file: the ambiguity guard fired on nested keys that
// findKey had already excluded by depth, and Claude Code is precisely the client that has
// them. The plan already resolved which key it meant, so the offset settles it. The golden
// fixture had no projects map, which is why this went unnoticed until a live run.
func TestARepeatedNestedContainerKeyDoesNotBlockTheTopLevelEdit(t *testing.T) {
	const body = `{
  "numStartups": 412,
  "mcpServers": {
    "notion": {
      "type": "http",
      "url": "https://mcp.notion.com/mcp"
    }
  },
  "projects": {
    "/home/u/one": {
      "mcpServers": {},
      "disabledMcpjsonServers": []
    },
    "/home/u/two": {
      "mcpServers": {}
    }
  }
}
`
	f := newFixture(t, "claude", body)
	f.install(t)

	got := parse(t, f.read(t))
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("no top-level mcpServers after install; keys are %v", keysOf(got))
	}
	if keys := keysOf(servers); len(keys) != 1 || keys[0] != ServerName {
		t.Errorf("top-level mcpServers = %v, want only %q", keys, ServerName)
	}
	for key := range got {
		if strings.HasPrefix(key, "_mcpd") {
			t.Errorf("install wrote %q into the client's own file", key)
		}
	}
	// Every project keeps its own container. Those are the client's per-project records and
	// touching one would strand it under a key the client does not read.
	projects, _ := got["projects"].(map[string]any)
	if len(projects) != 2 {
		t.Fatalf("projects = %v, want 2 entries", keysOf(projects))
	}
	for name, raw := range projects {
		p, _ := raw.(map[string]any)
		if _, ok := p["mcpServers"]; !ok {
			t.Errorf("project %q lost its own mcpServers", name)
		}
	}

	// Revert has to survive the same repetition, and carrying over a server declared after
	// installing is the path that addresses the container by name a second time.
	after := f.read(t)
	const anchor = `"mcpServers": {`
	after = strings.Replace(after, anchor, anchor+"\n    \"mine\": { \"type\": \"http\", \"url\": \"https://mine.test/mcp\" },", 1)
	if err := os.WriteFile(f.client.Path, []byte(after), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.revert(t)

	back := parse(t, f.read(t))
	block, ok := back["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("revert left no mcpServers; keys are %v", keysOf(back))
	}
	for _, want := range []string{"notion", "mine"} {
		if _, ok := block[want]; !ok {
			t.Errorf("revert lost %q", want)
		}
	}
	if _, ok := block[ServerName]; ok {
		t.Error("revert left mcpd behind")
	}
	if _, ok := back[StashKey]; ok {
		t.Error("revert left the stash key behind")
	}
}

// Renaming the server headers grows the file, so an append addressed by the original length
// lands short of the true end. On a real config it spliced mcpd's table into the middle of an
// args = [...] line, leaving a truncated key and a config Codex could not load at all: every
// server in the file went away, which is the widest possible blast radius for this package.
func TestTheAppendedTableLandsAtTheEndAfterTheRenamesGrewTheFile(t *testing.T) {
	const body = `[mcp_servers.alpha]
command = "alpha-server"

[mcp_servers.omega]
command = "npx"
args = ["-y", "omega@latest"]
`
	f := newFixture(t, "codex", body)
	f.install(t)
	got := f.read(t)

	const last = `args = ["-y", "omega@latest"]`
	if !strings.Contains(got, last) {
		t.Errorf("the final declaration was split by the appended table:\n%s", got)
	}
	if strings.Index(got, "[mcp_servers."+ServerName+"]") < strings.Index(got, last) {
		t.Errorf("the appended table landed before the end of the existing content:\n%s", got)
	}
}

// The one guard here that does not depend on this package's own arithmetic. Every other check
// re-reads the offsets that produced the bytes, so a mistake in them validates itself: a real
// append addressed against the wrong length spliced a table into the middle of a key, and the
// result was written out, leaving Codex unable to load any server in the file. A refusal has to
// leave nothing behind either, because a backup beside an untouched file only invites a
// needless restore.
func TestAChangeThatWouldLeaveTheFileUnparseableIsRefused(t *testing.T) {
	f := newFixture(t, "codex", codexGolden)
	p, err := f.client.PlanInstall("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	// Stands in for an offset mistake: text that lands somewhere it cannot be read.
	p.edits = append(p.edits, edit{Address: "corruption", To: "oops not toml\n", At: 0})

	if err := f.client.Apply(f.state, p); err == nil {
		t.Fatal("Apply accepted a change that leaves the file unparseable")
	}
	if got := f.read(t); got != codexGolden {
		t.Errorf("the file was written anyway:\n%s", got)
	}
	entries, err := os.ReadDir(filepath.Dir(f.client.Path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".mcpd-") || strings.Contains(e.Name(), ".bak") {
			t.Errorf("a refusal left %s behind", e.Name())
		}
	}
}

// A file that was already unparseable is the user's to fix. Saying so is the difference between
// a refusal they can act on and one that reads as mcpd having broken their config.
func TestAnAlreadyUnparseableFileIsNotBlamedOnTheTool(t *testing.T) {
	// The broken line sits outside any server table on purpose. Inside one it would be commented
	// out along with the rest of the block, and the result would parse: this tool can make an
	// unparseable file parse, which is not the branch under test.
	f := newFixture(t, "codex", "oops\n[mcp_servers.alpha]\ncommand = \"a\"\n")
	p, err := f.client.PlanInstall("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	err = f.client.Apply(f.state, p)
	if err == nil {
		t.Fatal("Apply accepted an already-unparseable file")
	}
	if !strings.Contains(err.Error(), "before this change either") {
		t.Errorf("refusal does not say the file was already broken: %v", err)
	}
}

// A receipt written before the displaced text moved out of the client's file still reverts. Three
// such receipts existed on this machine when the scheme changed, and a revert that could not read
// them would strand the very declarations it exists to restore, with the client's own file the only
// remaining copy and mcpd refusing to touch it.
func TestAReceiptFromTheInFileStashSchemeStillReverts(t *testing.T) {
	// The file as the old scheme left it: mcpd in the container, the user's servers under a key
	// mcpd invented.
	const installed = `{
  "mcpServers": {
    "mcpd": {
      "type": "http",
      "url": "http://127.0.0.1:7420/mcp/passthrough"
    }
  },
  "_mcpd_stashed": {
    "notion": {
      "type": "http",
      "url": "https://mcp.notion.com/mcp"
    }
  },
  "hasSeenTasksHint": true
}
`
	f := newFixture(t, "claude", installed)
	// A receipt from that scheme carries no displaced text, because the file held it.
	if err := writeReceipt(f.state, Receipt{
		Client: "claude", Path: f.client.Path,
		Endpoint: "http://127.0.0.1:7420/mcp/passthrough", InstallAt: "2026-07-31T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeReceipt: %v", err)
	}

	f.revert(t)

	got := parse(t, f.read(t))
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers is gone after revert; keys are %v", keysOf(got))
	}
	if _, ok := servers["notion"]; !ok {
		t.Errorf("revert lost the stashed declaration: %v", keysOf(servers))
	}
	if _, ok := servers[ServerName]; ok {
		t.Error("revert left mcpd behind")
	}
	if _, ok := got[StashKey]; ok {
		t.Errorf("revert left %q behind", StashKey)
	}
}

func TestJSONInstallAndRevertWithoutExistingContainer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		container string
		body      string
	}{
		{name: "claude", container: "mcpServers", body: "{\n  \"theme\": \"dark\"\n}\n"},
		{name: "cursor", container: "mcpServers", body: "{\n  \"theme\": \"dark\"\n}\n"},
		{name: "opencode", container: "mcp", body: "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"theme\": \"dark\"\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.name, tc.body)
			f.install(t)

			doc := parse(t, f.read(t))
			container, ok := doc[tc.container].(map[string]any)
			if !ok {
				t.Fatalf("%q is missing or not an object after install", tc.container)
			}
			entry, ok := container[ServerName].(map[string]any)
			if !ok {
				t.Fatalf("%s.%s is missing or not an object", tc.container, ServerName)
			}
			if got, want := entry["url"], f.client.Endpoint("127.0.0.1:7420"); got != want {
				t.Errorf("%s.%s url = %v, want %s", tc.container, ServerName, got, want)
			}

			f.revert(t)
			if got := f.read(t); got != tc.body {
				t.Errorf("revert is not byte-for-byte:\n--- got ---\n%s\n--- want ---\n%s", got, tc.body)
			}
		})
	}
}

func TestApplyAndRevertRefuseFilesChangedSinceInspection(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		f := newFixture(t, "cursor", cursorGolden)
		p, err := f.client.PlanInstall("127.0.0.1:7420")
		if err != nil {
			t.Fatalf("PlanInstall: %v", err)
		}
		if err := os.WriteFile(f.client.Path, []byte(f.body+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := f.client.Apply(f.state, p); err == nil || !strings.Contains(err.Error(), "changed since it was inspected") {
			t.Fatalf("Apply = %v, want changed-since-inspection refusal", err)
		}
	})

	t.Run("revert", func(t *testing.T) {
		f := newFixture(t, "cursor", cursorGolden)
		f.install(t)
		p, err := f.client.PlanRevert(f.state)
		if err != nil {
			t.Fatalf("PlanRevert: %v", err)
		}
		if err := os.WriteFile(f.client.Path, []byte(f.read(t)+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := f.client.Revert(f.state, p); err == nil || !strings.Contains(err.Error(), "changed since it was inspected") {
			t.Fatalf("Revert = %v, want changed-since-inspection refusal", err)
		}
	})
}

func TestApplyEditsRefusesInvalidAnchors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		edit edit
		want string
	}{
		{name: "out of bounds insertion", body: "abc", edit: edit{Address: "insert", To: "x", At: 4}, want: "outside the file"},
		{name: "offset text mismatch", body: "abc", edit: edit{Address: "replace", From: "z", To: "x", At: 1}, want: "no longer what the plan resolved"},
		{name: "non-unique anchor", body: "abcabc", edit: edit{Address: "replace", From: "abc", To: "x"}, want: "found 2 times, want 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applyEdits(tc.body, []edit{tc.edit})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("applyEdits = %v, want refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestRevertCleansReceiptWhenInstallWasNeverApplied(t *testing.T) {
	f := newFixture(t, "cursor", cursorGolden)
	p, err := f.client.PlanInstall("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if err := writeReceipt(f.state, Receipt{
		Client: f.client.Name, Path: f.client.Path, Endpoint: p.Endpoint,
		InstallAt: "2026-08-01T00:00:00Z", Edits: p.edits, Displaced: p.displaced,
		OriginalHash: contentHash([]byte(f.body)),
	}); err != nil {
		t.Fatalf("writeReceipt: %v", err)
	}

	revert, err := f.client.PlanRevert(f.state)
	if err != nil {
		t.Fatalf("PlanRevert: %v", err)
	}
	if !revert.Empty() {
		t.Fatalf("PlanRevert returned edits for an install that was never applied: %v", revert.Notes)
	}
	if err := f.client.Revert(f.state, revert); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := f.read(t); got != f.body {
		t.Errorf("revert changed an untouched client file:\n%s", got)
	}
	if _, err := os.Stat(receiptPath(f.state, f.client.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("receipt still exists after cleanup: %v", err)
	}
}

func TestTOMLMultilineStringIsNotTreatedAsAServerBlock(t *testing.T) {
	const body = `model = "gpt-5.6-terra"
description = """
before
[mcp_servers.not-a-table]
after
"""

[projects."/home/user"]
trust_level = "trusted"
`
	f := newFixture(t, "codex", body)
	f.install(t)

	got := f.read(t)
	owned := "\n[mcp_servers.mcpd]\nurl = \"http://127.0.0.1:7420/mcp/passthrough\"\n"
	outside, found := strings.CutSuffix(got, owned)
	if !found {
		t.Fatalf("installed file does not end with the mcpd-owned region:\n%s", got)
	}
	if outside != body {
		t.Errorf("install changed bytes outside the mcpd-owned region:\n--- got ---\n%s\n--- want ---\n%s", outside, body)
	}
	if !strings.Contains(got, "description = \"\"\"\nbefore\n[mcp_servers.not-a-table]\nafter\n\"\"\"") {
		t.Errorf("the multi-line string was corrupted:\n%s", got)
	}
}
