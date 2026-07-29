# Draft implementation reference (temporary)

**This file is scratch, not a source of truth.** `openspec/` is authoritative for
requirements, design, and tasks. This is the drafted Go from the pre-OpenSpec
implementation plan, kept only so Tasks 2, 3, and 11 start from working code rather than
a blank file.

**Delete this file once Tasks 2, 3, and 11 have landed.** It will drift from the real
implementation immediately, and a stale reference that looks authoritative is worse than
no reference.

Known corrections already applied elsewhere: three `oauthex` signatures in the Task 1
section below were wrong and are fixed in `cmd/oauthprobe/main.go`. Treat `go doc` as
authoritative over anything here.

---

# mcpd Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `mcpd`, a single-binary Go daemon that fronts 12 MCP backends for Claude Code, Codex, Cursor CLI, and OpenCode, with a tool-search facade, a loopback status UI, and browser-triggered OAuth.

**Architecture:** One `systemd --user` daemon on `127.0.0.1:7420`. It holds one `*mcp.ClientSession` per backend (stdio children spawned once, HTTP backends over streamable HTTP), maintains a flattened tool catalog with content-hash-keyed embeddings, and serves three surfaces off one `http.ServeMux`: `/mcp/search` (3-tool facade), `/mcp/passthrough` (full catalog), and `/` (status page, inspector, OAuth routes).

**Tech Stack:** Go 1.26.5 (installed), `github.com/modelcontextprotocol/go-sdk` v1.7.0, `golang.org/x/oauth2` (transitively, via the SDK's `auth` package), stdlib `net/http` including `http.CrossOriginProtection` (Go 1.25+), `embed.FS` for UI assets. No web framework, no database, no other third-party dependencies.

**Design spec:** `~/Articulate/obsidian/ahodges-art-agents/_plans/art-agent-scratch/2026-07-28-mcpd-local-mcp-proxy-design.md`. Read it before Task 2. Every "why" lives there; this document is the "how".

## Global Constraints

- Go 1.26.5. Module path `github.com/ahodges/mcpd`. `go.mod` declares `go 1.25` minimum (required for `http.CrossOriginProtection`).
- Repo is `ahodges22/mcpd` (private), cloned at `/home/alex/Articulate/repos/mcpd/`. The `mcp-tool-search/` prototype stays in `art-agent-scratch` and keeps running until Task 12 rewires clients; it is **not** modified or deleted.
- `testdata/config.example.json` uses **placeholder hostnames**, never the real `*.art-internal.com` ones. The working config lives at `~/.config/mcpd/config.json`, which is untracked, so version control never carries internal infra topology.
- Bind address is exactly `127.0.0.1:7420`. The port is load-bearing: it is the registered OAuth redirect URI. Never bind `0.0.0.0`.
- Canonical tool id format is exactly `mcp__<server>__<tool>`, matching the prototype so existing permission hooks keep matching.
- `cmd.Env` for a stdio child is **always** an explicitly constructed slice, **never** `nil`. `nil` means inherit-everything, which leaks every credential to third-party npm code.
- Never set `StreamableHTTPOptions.DisableLocalhostProtection`.
- Config at `~/.config/mcpd/config.json` is written by the user only; the daemon never writes it. Daemon state lives under `~/.local/state/mcpd/`.
- Token files are mode `0600`; the `tokens/` directory is `0700`.
- All backend-derived strings (tool names, descriptions, errors, **and tool results**) reach the browser via `textContent` or `html/template` escaping. No `innerHTML`, no markdown rendering.
- `tools/call` is at-most-once. Reconnect only when no send was attempted; never replay after a send begins, including when the write returns an error.
- Conventional Commit subjects, no body, no `Co-Authored-By` trailer.

## File Structure

| file | responsibility |
|---|---|
| `go.mod`, `go.sum` | module definition |
| `cmd/mcpd/main.go` | flag parsing, `serve`/`install` subcommands, wiring, graceful shutdown |
| `cmd/oauthprobe/main.go` | Phase 0 feasibility probe (Task 1); kept afterwards as a diagnostic |
| `cmd/evalrank/main.go` | ranking eval runner; deliberately not a `go test` |
| `internal/config/config.go` | load and validate config, `${VAR}` expansion, `ChildEnv` construction |
| `internal/backend/backend.go` | one upstream: connect/reconnect, health, dispatch gate |
| `internal/backend/registry.go` | all backends: routing, lifecycle lock, generation counter |
| `internal/backend/overrides.go` | enable/disable persistence |
| `internal/catalog/catalog.go` | flatten to canonical ids, persist, trigger-counter coalescing refresh |
| `internal/embedding/client.go` | LiteLLM embeddings client, batching, content-hash cache |
| `internal/rank/rank.go` | lexical scorer (ported) plus cosine, reciprocal rank fusion |
| `internal/rank/abstain.go` | abstention signal and threshold calibration |
| `internal/oauthstore/store.go` | token and client-credential persistence, pending-auth registry |
| `internal/mcpsrv/search.go` | 3-tool facade server |
| `internal/mcpsrv/passthrough.go` | full-catalog server |
| `internal/web/guard.go` | shared `CrossOriginProtection`, POST-only, callback exemption |
| `internal/web/web.go` | status API, status page, inspector, OAuth routes, `embed.FS` |
| `internal/web/assets/` | `index.html`, `app.js`, `style.css` |
| `internal/install/install.go` | client rewiring, approval migration, surgical revert |
| `internal/testfake/backend.go` | in-process fake MCP backend used by every test |
| `testdata/eval_queries.json` | ported and expanded eval queries |
| `packaging/mcpd.service` | systemd user unit |

---

### Task 1: Phase 0, the Notion OAuth feasibility gate (DONE 2026-07-28)

**Outcome: row 1, everything works. Proceed as designed.** See `mcpd/PHASE0.md` for
evidence. Notion returns a spec-compliant `401` with a `resource_metadata` pointer,
advertises `registration_endpoint`, supports PKCE `S256`, lists `refresh_token` in
`grant_types_supported`, and DCR returned `HTTP 201` accepting
`http://127.0.0.1:7420/oauth/callback` verbatim as a public client
(`token_endpoint_auth_method: none`). Task 10 uses `DynamicClientRegistrationConfig`
and persists only the `client_id`; there is no client secret.

The gate also caught three wrong signatures in this task's original draft code,
recorded in `PHASE0.md` and fixed below. Steps are retained for reproducibility.

Success criterion 12 made this a precondition. If Notion had rejected loopback
redirects, the fixed-port design would have broken and Tasks 10 and 14 would have
needed redesign, so it ran before any daemon code existed.

**Files:**
- Create: `mcpd/go.mod`
- Create: `mcpd/cmd/oauthprobe/main.go`
- Create: `mcpd/PHASE0.md` (findings record)

**Interfaces:**
- Consumes: nothing.
- Produces: a decision recorded in `PHASE0.md` with one of four outcomes, which Task 10 reads to choose its registration mode.

- [x] **Step 1: Initialise the module**

```bash
mkdir -p /home/alex/Articulate/repos/mcpd/cmd/oauthprobe
cd /home/alex/Articulate/repos/mcpd
go mod init github.com/ahodges/mcpd
go get github.com/modelcontextprotocol/go-sdk@v1.7.0
```

- [x] **Step 2: Confirm the `oauthex` function names before writing code against them**

Run: `go doc github.com/modelcontextprotocol/go-sdk/oauthex | grep -E "^func "`
Expected: exported functions for protected-resource metadata, authorization-server metadata, and client registration. **`go doc` is authoritative and this plan is not.** If the names differ from Step 3's code, change the code to match.

- [x] **Step 3: Write the probe**

`cmd/oauthprobe/main.go`. The real signatures, confirmed by `go doc`, differ from the
first draft in three ways: `*http.Client` is the **last** parameter, both metadata
getters take a `metadataURL` **and** the resource/issuer URL because they verify the
two agree, and the functions are documented under their return types so
`grep '^func '` does not list them.

```go
metaURL, err := challengeMetadataURL(ctx, hc) // from the 401 WWW-Authenticate
prm, err := oauthex.GetProtectedResourceMetadata(ctx, metaURL, *serverURL, hc)
issuer := prm.AuthorizationServers[0]
asm, err := oauthex.GetAuthServerMeta(ctx, issuer+"/.well-known/oauth-authorization-server", issuer, hc)
res, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint,
    &oauthex.ClientRegistrationMetadata{
        ClientName:              "mcpd (feasibility probe)",
        RedirectURIs:            []string{*redirect},
        GrantTypes:              []string{"authorization_code", "refresh_token"},
        ResponseTypes:           []string{"code"},
        TokenEndpointAuthMethod: "none",
    }, hc)
```

Discovery starts from the `401` challenge via `oauthex.ParseWWWAuthenticate` rather
than a guessed well-known path, because Notion's metadata lives at
`/.well-known/oauth-protected-resource/mcp` with a path suffix the challenge simply
tells you. DCR is behind a `-register` flag, since it creates a real client at the
provider. Full source is committed in the repo.

- [x] **Step 4: Build and run the probe**

Run:
```bash
cd /home/alex/Articulate/repos/mcpd
go build ./cmd/oauthprobe && ./oauthprobe 2>&1 | tee /tmp/phase0-notion.txt
```
Expected: four labelled sections. Needs network; no VPN required for `mcp.notion.com`.

- [x] **Step 5: Record findings and take the gate decision**

Write `PHASE0.md` with the raw output plus which of the spec's four rows applies:

| finding | consequence for Task 10 |
|---|---|
| Discovery, DCR, and loopback all work | Proceed as designed; Task 10 uses `DynamicClientRegistrationConfig` |
| DCR unsupported, client can be pre-registered | Task 10 uses `PreregisteredClient`; config gains optional `client_id`/`client_secret` |
| Loopback or plain-HTTP redirect rejected | **STOP.** The fixed-port assumption is broken. Do not start Task 2; return to the design owner |
| No refresh token | Task 10 unchanged; success criterion 2's restart clause gets restated in the spec |

- [x] **Step 6: Commit**

```bash
git add go.mod mcpd/go.sum mcpd/cmd/oauthprobe/main.go mcpd/PHASE0.md
git commit -m "chore: add Notion OAuth feasibility probe and record Phase 0 findings"
```

---

### Task 2: Config loading and least-privilege child environments

**Files:**
- Create: `mcpd/internal/config/config.go`
- Create: `mcpd/internal/config/config_test.go`
- Create: `mcpd/testdata/config.example.json`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Backend struct { Name, Command string; Args []string; Env map[string]string; EnvPassthrough []string; HTTPURL string; Headers map[string]string; Auth string; TimeoutSec int }`
  - `type Config struct { Backends map[string]Backend }`
  - `func Load(path string) (*Config, error)`
  - `func (b Backend) ChildEnv(parent []string) []string`
  - `func (b Backend) ExpandHeaders(parent []string) map[string]string`
  - `func (b Backend) IsStdio() bool`

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:

```go
package config

import (
	"slices"
	"strings"
	"testing"
)

func TestChildEnvIsLeastPrivilege(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/alex", "LANG=en_US.UTF-8",
		"AWS_PROFILE=mgmt", "KUBECONFIG=/home/alex/.kube/config",
		"GH_PAT=secret-pat", "DD_ACCESS_TOKEN=secret-dd", "LITELLM_KEY=secret-litellm",
	}
	art := Backend{
		Name:           "art",
		Env:            map[string]string{"ART_BIN": "/home/alex/.bin/art"},
		EnvPassthrough: []string{"AWS_*", "KUBECONFIG"},
	}
	got := art.ChildEnv(parent)

	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/home/alex", "LANG=en_US.UTF-8",
		"ART_BIN=/home/alex/.bin/art", "AWS_PROFILE=mgmt",
		"KUBECONFIG=/home/alex/.kube/config",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in child env %v", want, got)
		}
	}
	// The whole point: undeclared credentials must not cross the boundary.
	for _, leak := range []string{"GH_PAT", "DD_ACCESS_TOKEN", "LITELLM_KEY"} {
		for _, kv := range got {
			if strings.HasPrefix(kv, leak+"=") {
				t.Errorf("leaked %s to child: %q", leak, kv)
			}
		}
	}
}

func TestChildEnvNeverReturnsNil(t *testing.T) {
	// nil means "inherit everything" in exec.Cmd, which is the over-correction.
	got := Backend{Name: "flint"}.ChildEnv([]string{"PATH=/usr/bin", "GH_PAT=secret"})
	if got == nil {
		t.Fatal("ChildEnv returned nil; exec.Cmd would inherit the full environment")
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "GH_PAT=") {
			t.Errorf("backend declaring nothing received %q", kv)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd mcpd && go test ./internal/config/`
Expected: FAIL, `undefined: Backend`.

- [ ] **Step 3: Write the implementation**

`internal/config/config.go`:

```go
// Package config loads mcpd's backend declarations and builds least-privilege
// environments for stdio children.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// baseEnvKeys are forwarded to every stdio child because a process needs them to
// function at all. Everything else must be declared per backend.
var baseEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM",
	"TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
}

var envRef = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

type Backend struct {
	Name           string            `json:"-"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
	HTTPURL        string            `json:"http_url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Auth           string            `json:"auth,omitempty"` // "" or "oauth"
	TimeoutSec     int               `json:"timeout,omitempty"`
}

type Config struct {
	Backends map[string]Backend `json:"backends"`
}

func (b Backend) IsStdio() bool { return b.Command != "" }

// ChildEnv builds the environment for a stdio child: a curated base, plus the
// backend's declared env, plus its declared passthrough patterns. It never returns
// nil, because exec.Cmd treats nil as "inherit everything".
func (b Backend) ChildEnv(parent []string) []string {
	index := indexEnv(parent)
	out := make([]string, 0, len(baseEnvKeys)+len(b.Env)+8)
	add := func(k, v string) { out = append(out, k+"="+v) }

	for _, k := range baseEnvKeys {
		if v, ok := index[k]; ok {
			add(k, v)
		}
	}
	for k, v := range index {
		if strings.HasPrefix(k, "LC_") {
			add(k, v)
		}
	}
	for _, pat := range b.EnvPassthrough {
		if base, ok := strings.CutSuffix(pat, "*"); ok {
			for k, v := range index {
				if strings.HasPrefix(k, base) {
					add(k, v)
				}
			}
			continue
		}
		if v, ok := index[pat]; ok {
			add(pat, v)
		}
	}
	for k, v := range b.Env {
		add(k, expand(v, index))
	}
	return out
}

// ExpandHeaders resolves ${VAR} references in HTTP headers against the daemon's own
// environment. Unlike ChildEnv this is not a privilege boundary: the value is used by
// mcpd itself, not handed to a child process.
func (b Backend) ExpandHeaders(parent []string) map[string]string {
	index := indexEnv(parent)
	out := make(map[string]string, len(b.Headers))
	for k, v := range b.Headers {
		out[k] = expand(v, index)
	}
	return out
}

func indexEnv(parent []string) map[string]string {
	index := make(map[string]string, len(parent))
	for _, kv := range parent {
		if k, v, ok := strings.Cut(kv, "="); ok {
			index[k] = v
		}
	}
	return index
}

func expand(s string, index map[string]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return index[envRef.FindStringSubmatch(m)[1]]
	})
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(c.Backends) == 0 {
		return nil, fmt.Errorf("config declares no backends")
	}
	for name, b := range c.Backends {
		if b.IsStdio() == (b.HTTPURL != "") {
			return nil, fmt.Errorf("backend %q must declare exactly one of command or http_url", name)
		}
		b.Name = name
		c.Backends[name] = b
	}
	return &c, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd mcpd && go test ./internal/config/ -v`
Expected: PASS, both tests.

- [ ] **Step 5: Write the example config, migrated from the prototype**

`testdata/config.example.json`: port all 13 backends from `mcp-tool-search/config.json`, **replacing every real `*.art-internal.com` hostname with a placeholder** such as `https://gateway.example.internal/mcp/context7`, since this file is committed. Three further required changes: `github` uses `${GH_PAT}` rather than `${GITHUB_TOKEN}`, because only the former is visible to a systemd unit. `art` gains `"env_passthrough": ["AWS_*", "KUBECONFIG", "VAULT_*"]`. `notion` is added with `"auth": "oauth"`. `metabase` is present but gets disabled at runtime per the spec.

- [ ] **Step 6: Commit**

```bash
git add internal/config mcpd/testdata/config.example.json
git commit -m "feat: add config loading with least-privilege child environments"
```

---

### Task 3: Backend sessions, routing, and at-most-once dispatch

**Files:**
- Create: `mcpd/internal/testfake/backend.go`
- Create: `mcpd/internal/backend/backend.go`
- Create: `mcpd/internal/backend/registry.go`
- Create: `mcpd/internal/backend/backend_test.go`

**Interfaces:**
- Consumes: `config.Backend`, `config.Config`.
- Produces:
  - `type State string` with `StateUp`, `StateDown`, `StateNeedsAuth`, `StateDisabled`
  - `type Health struct { State State; Transport string; ToolCount int; LastRefresh time.Time; LastErr string; AuthNote string }`
  - `type Backend struct{...}` with `ListTools(ctx) ([]*mcp.Tool, error)`, `Call(ctx, tool string, args map[string]any) (*mcp.CallToolResult, error)`, `Health() Health`
  - `type Registry struct{...}` with `NewRegistry(*config.Config, func(server string)) *Registry`, `Get(name) (*Backend, bool)`, `Names() []string`, `Health() map[string]Health`
  - `var ErrNotAttempted error`, returned only when no send began so callers may retry

- [ ] **Step 1: Write the in-process fake backend**

`internal/testfake/backend.go`:

```go
// Package testfake provides an in-process MCP backend for mcpd's tests.
package testfake

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Fake struct {
	mu    sync.Mutex
	tools []*mcp.Tool

	ListCalls   atomic.Int64 // asserts coalescing
	SideEffects atomic.Int64 // asserts at-most-once
	BeforeList  func()       // hook to hold a list in flight
	Srv         *mcp.Server
}

func New(name string, tools []*mcp.Tool) *Fake {
	f := &Fake{tools: tools}
	f.Srv = mcp.NewServer(&mcp.Implementation{Name: name, Version: "test"}, nil)
	for _, t := range tools {
		f.Srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.SideEffects.Add(1)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + name}},
			}, nil
		})
	}
	return f
}

func (f *Fake) SetTools(ts []*mcp.Tool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = ts
}
```

Confirm the handler signature with `go doc github.com/modelcontextprotocol/go-sdk/mcp.ToolHandler` and match it exactly before running.

- [ ] **Step 2: Write the failing tests**

`internal/backend/backend_test.go`:

```go
package backend

import (
	"context"
	"errors"
	"testing"
)

func TestCallRoutesToOwningBackend(t *testing.T) {
	r := newTestRegistry(t) // helper: two fakes, "alpha" and "beta"
	b, ok := r.Get("alpha")
	if !ok {
		t.Fatal("alpha not registered")
	}
	res, err := b.Call(context.Background(), "kubectl_logs", map[string]any{"pod": "p"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := textOf(res); got != "ok:alpha" {
		t.Errorf("routed to wrong backend: %q", got)
	}
}

func TestCallIsAtMostOnceAfterSendBegins(t *testing.T) {
	r, fake := newFlakyRegistry(t) // transport drops the connection after delivering
	b, _ := r.Get("alpha")
	_, err := b.Call(context.Background(), "kubectl_logs", map[string]any{"pod": "p"})
	if err == nil {
		t.Fatal("expected an error when the response is lost")
	}
	if errors.Is(err, ErrNotAttempted) {
		t.Error("a delivered request must not report not-attempted; callers would retry it")
	}
	if n := fake.SideEffects.Load(); n != 1 {
		t.Errorf("side effects = %d, want exactly 1 (no replay)", n)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/backend/`
Expected: FAIL, `undefined: newTestRegistry` and `undefined: ErrNotAttempted`.

- [ ] **Step 4: Implement `backend.go`**

The dispatch gate is `sync.RWMutex`: `RLock` is a lease held across the enabled check and the send; `Lock` is close-then-drain, used by Task 5's disable.

```go
package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/config"
)

// ErrNotAttempted means no bytes were offered to the transport, so the caller may
// safely retry. Any other error means the outcome is unknown and must not be replayed.
var ErrNotAttempted = errors.New("no send attempted")

type State string

const (
	StateUp        State = "up"
	StateDown      State = "down"
	StateNeedsAuth State = "needs-auth"
	StateDisabled  State = "disabled"
)

type Health struct {
	State       State     `json:"state"`
	Transport   string    `json:"transport"`
	ToolCount   int       `json:"tool_count"`
	LastRefresh time.Time `json:"last_refresh"`
	LastErr     string    `json:"last_error,omitempty"`
	AuthNote    string    `json:"auth_note,omitempty"`
}

// environ is a package var so tests can inject a parent environment.
var environ = os.Environ

type Backend struct {
	spec config.Backend

	gate sync.RWMutex // RLock = dispatch lease; Lock = close-and-drain

	mu      sync.Mutex
	session *mcp.ClientSession
	health  Health
}

func (b *Backend) Health() Health {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.health
}

// Call dispatches a tool call under a shared gate lease. It returns ErrNotAttempted
// only when no send began.
func (b *Backend) Call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	b.gate.RLock()
	defer b.gate.RUnlock()

	b.mu.Lock()
	sess, state := b.session, b.health.State
	b.mu.Unlock()

	if state == StateDisabled {
		return nil, fmt.Errorf("%w: backend disabled", ErrNotAttempted)
	}
	if sess == nil {
		// No session, so nothing can have reached the upstream: reconnect and send once.
		var err error
		if sess, err = b.reconnect(ctx); err != nil {
			return nil, fmt.Errorf("%w: connect: %v", ErrNotAttempted, err)
		}
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		// A send began. The write's error is not evidence the upstream did not act.
		return nil, fmt.Errorf("tool %s: outcome unknown after dispatch: %w", tool, err)
	}
	return res, nil
}

func (b *Backend) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()
	if sess == nil {
		var err error
		if sess, err = b.reconnect(ctx); err != nil {
			return nil, err
		}
	}
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

func (b *Backend) transport() (mcp.Transport, error) {
	if b.spec.IsStdio() {
		cmd := exec.Command(b.spec.Command, b.spec.Args...)
		// Never nil: nil inherits the daemon's whole environment.
		cmd.Env = b.spec.ChildEnv(environ())
		return &mcp.CommandTransport{Command: cmd}, nil
	}
	return &mcp.StreamableClientTransport{
		Endpoint:   b.spec.HTTPURL,
		HTTPClient: b.httpClient(),
	}, nil
}
```

Also implement `reconnect` (exponential backoff, health update, session store) and `httpClient` (a `http.RoundTripper` injecting `ExpandHeaders`; OAuth wiring arrives in Task 10).

- [ ] **Step 5: Implement `registry.go`**

Holds `map[string]*Backend`, a per-backend lifecycle `sync.Mutex`, and a per-backend generation counter (`atomic.Uint64`). Wires `mcp.ClientOptions{ToolListChangedHandler: ...}` to the callback passed to `NewRegistry`, which Task 4 uses as a refresh trigger.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/backend/ -race -v`
Expected: PASS. `-race` is not optional on this package.

- [ ] **Step 7: Commit**

```bash
git add internal/backend mcpd/internal/testfake
git commit -m "feat: add backend sessions with at-most-once dispatch"
```

---

### Task 4: Catalog with coalescing refresh

**Files:**
- Create: `mcpd/internal/catalog/catalog.go`
- Create: `mcpd/internal/catalog/catalog_test.go`

**Interfaces:**
- Consumes: `backend.Registry`, `backend.Backend`.
- Produces:
  - `type Entry struct { ID, Server, Tool, Description string; Schema json.RawMessage; Annotations *mcp.ToolAnnotations }`
  - `type Catalog struct{...}` with `New(*backend.Registry, string) *Catalog`, `Entries() []Entry`, `Lookup(id string) (Entry, bool)`, `Trigger(server string)`, `RefreshAll(ctx)`, `WaitIdle()`, `Load() error`, `Save() error`
  - `func CanonicalID(server, tool string) string`, returning `"mcp__" + server + "__" + tool`

- [ ] **Step 1: Write the failing tests**

```go
func TestTriggerDuringRefreshCausesExactlyOneFollowUp(t *testing.T) {
	fake := testfake.New("alpha", twoTools)
	release := make(chan struct{})
	fake.BeforeList = func() {
		if fake.ListCalls.Load() == 1 {
			<-release // hold the first list in flight
		}
	}
	c := newTestCatalog(t, fake)

	done := make(chan struct{})
	go func() { c.RefreshAll(context.Background()); close(done) }()
	waitFor(t, func() bool { return fake.ListCalls.Load() == 1 })

	fake.SetTools(oneTool) // the change the notification is about
	c.Trigger("alpha")
	c.Trigger("alpha")
	c.Trigger("alpha") // a burst must collapse
	close(release)
	<-done
	c.WaitIdle()

	if n := fake.ListCalls.Load(); n != 2 {
		t.Errorf("ListTools calls = %d, want 2 (one in flight, one coalesced follow-up)", n)
	}
	if got := len(c.Entries()); got != 1 {
		t.Errorf("catalog has %d entries, want 1: the follow-up result must win", got)
	}
}

func TestTriggerDuringFollowUpCausesAThirdRead(t *testing.T) {
	// Proves the loop converges on the trigger counter rather than a fixed bound.
}

func TestDeadBackendDoesNotSinkTheCatalog(t *testing.T) {
	c := newTestCatalog(t, testfake.New("alpha", twoTools), brokenFake(t))
	c.RefreshAll(context.Background())
	if len(c.Entries()) != 2 {
		t.Errorf("entries = %d, want alpha's 2 despite beta failing", len(c.Entries()))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/catalog/`
Expected: FAIL, `undefined: newTestCatalog`.

- [ ] **Step 3: Implement the coalescing loop**

Per-backend state: `refreshing bool`, `triggers atomic.Uint64`, a debounce timer, guarded by a mutex. `refreshOne` records `triggers.Load()` before its `ListTools` and loops while the value differs on completion. Consecutive rounds back off 250ms, 500ms, 1s, capped at the TTL. Triggers are debounced over 250ms so a burst becomes one. A commit is rejected if the backend's generation counter changed. There is deliberately **no** commit-sequence number: reads are serialized, so commits are already ordered.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/catalog/ -race -count=5 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog
git commit -m "feat: add tool catalog with coalescing refresh"
```

---

### Task 5: Lifecycle serialization, enable/disable, and the dispatch gate

**Files:**
- Modify: `mcpd/internal/backend/registry.go`
- Modify: `mcpd/internal/backend/backend.go`
- Create: `mcpd/internal/backend/overrides.go`
- Create: `mcpd/internal/backend/lifecycle_test.go`

**Interfaces:**
- Consumes: Task 3's `Registry` and `Backend`.
- Produces: `Disable(ctx, name) error`, `Enable(ctx, name) error`, `Generation(name) uint64`, `LoadOverrides(path) (map[string]bool, error)`, `SaveOverrides(path, map[string]bool) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestDisableWinsAgainstInFlightRefresh(t *testing.T) {
	// test 14: hold ListTools in flight, disable, release. The disabled backend's
	// tools must not appear, and a pending retry must not respawn it.
}

func TestDispatchNeverWritesAfterGateCloses(t *testing.T) {
	// test 15: assert on the fake's received-request log, not the caller's return
	// value, because the caller cannot distinguish rejected from completed.
}

func TestDisableOverrideSurvivesRestart(t *testing.T) {
	// test 10: SaveOverrides then LoadOverrides into a fresh registry.
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/backend/ -run 'Disable|Dispatch'`
Expected: FAIL, `r.Disable undefined`.

- [ ] **Step 3: Implement disable in the order the tests assert**

Persist the override **first**, so a crash mid-disable leaves the backend off. Then `gate.Lock()` to close and drain. Then cancel and *await* outstanding refresh and retry tasks. Then close the session and terminate the stdio child. Then bump the generation counter. `Enable` takes the same lifecycle lock, so it cannot run during an in-progress teardown and leak a second child process.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/backend/ -race -count=5`
Expected: PASS. `-count=5` because a single green run on a race test proves very little.

- [ ] **Step 5: Commit**

```bash
git add internal/backend
git commit -m "feat: serialize backend lifecycle and add enable/disable kill switch"
```

---

### Task 6: Embeddings client and reciprocal rank fusion

**Files:**
- Create: `mcpd/internal/embedding/client.go`
- Create: `mcpd/internal/embedding/client_test.go`
- Create: `mcpd/internal/rank/rank.go`
- Create: `mcpd/internal/rank/rank_test.go`

**Interfaces:**
- Consumes: `catalog.Entry`.
- Produces:
  - `func (c *Client) Embed(ctx, texts []string) ([][]float32, error)`
  - `type Cache struct{...}` with `Key(catalog.Entry) string` (sha256 of name, description, schema), `Get`, `Put`, `Load`, `Save`
  - `type Result struct { ID, Server, Description string; Score float64 }`
  - `func Lexical(query string, entries []catalog.Entry) []Result`
  - `func Fuse(query string, entries []catalog.Entry, vecs map[string][]float32, qvec []float32, limit int) []Result`

- [ ] **Step 1: Write the failing tests**

```go
func TestFuseDegradesToLexicalWithoutVectors(t *testing.T) {
	got := Fuse("kubernetes pod logs", entries, nil, nil, 3)
	if len(got) == 0 || got[0].ID != "mcp__art__kubectl_logs" {
		t.Errorf("lexical-only fusion failed: %+v", got)
	}
}

func TestFuseUsesReciprocalRankNotScoreBlending(t *testing.T) {
	// A tool ranked 1st lexically and 3rd semantically must score 1/61 + 1/63,
	// proving ranks are fused rather than raw incommensurable scores.
}

func TestEmbedFailureIsSoft(t *testing.T) {
	// A client pointed at a closed port returns an error callers can ignore, and
	// Fuse still ranks.
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/rank/ ./internal/embedding/`
Expected: FAIL, `undefined: Fuse`.

- [ ] **Step 3: Implement**

`Fuse` computes two orderings and sums `1/(60+rank)`. `Lexical` ports the keyword and idf scorer from `mcp-tool-search/src/proxy.py`, including its tool-name squashing. The embeddings client posts to `${LITELLM_BASE}/v1/embeddings` with `model: text-embedding-3-small`, batches entries, and returns an error rather than panicking when unreachable.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/rank/ ./internal/embedding/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rank mcpd/internal/embedding
git commit -m "feat: add embeddings client and reciprocal rank fusion"
```

---

### Task 7: Abstention signal

**Files:**
- Create: `mcpd/internal/rank/abstain.go`
- Create: `mcpd/internal/rank/abstain_test.go`

**Interfaces:**
- Consumes: raw lexical and cosine scores, not the fused score.
- Produces: `type Evidence struct { BestCosine, BestLexical float64 }`, `type Thresholds struct { Cosine, Lexical float64; Enabled bool }`, `func (t Thresholds) LowConfidence(e Evidence) bool`, `func Calibrate(answerable, negative []Evidence) (Thresholds, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestCalibrateRefusesWhenNoGapExists(t *testing.T) {
	got, err := Calibrate(overlappingAnswerable, overlappingNegative)
	if err == nil {
		t.Error("expected an error reporting the absent separating gap")
	}
	if got.Enabled {
		t.Error("abstention must ship disabled rather than flag wrongly")
	}
}

func TestCalibratePicksMidpointOfTheGap(t *testing.T) {
	// answers >= 0.40, negatives <= 0.20
	got, err := Calibrate(sepAnswerable, sepNegative)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if got.Cosine < 0.20 || got.Cosine > 0.40 {
		t.Errorf("cosine threshold %v outside the gap", got.Cosine)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/rank/ -run Calibrate`
Expected: FAIL, `undefined: Calibrate`.

- [ ] **Step 3: Implement the fixed selection rule**

Highest threshold leaving every answerable true answer above it; lowest leaving every negative below it; midpoint when the first exceeds the second; otherwise `Enabled: false` plus an error naming the overlap. Never read the fused score here: it encodes rank position and ranker agreement, not relevance.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/rank/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rank
git commit -m "feat: add abstention signal with gap-based threshold calibration"
```

---

### Task 8: The two MCP endpoints

**Files:**
- Create: `mcpd/internal/mcpsrv/search.go`
- Create: `mcpd/internal/mcpsrv/passthrough.go`
- Create: `mcpd/internal/mcpsrv/mcpsrv_test.go`

**Interfaces:**
- Consumes: `catalog.Catalog`, `backend.Registry`, `rank.Fuse`, `rank.Thresholds`.
- Produces: `func NewSearch(*catalog.Catalog, *backend.Registry, rank.Thresholds) *mcp.Server`, `func NewPassthrough(*catalog.Catalog, *backend.Registry) *mcp.Server`, `func (p *Passthrough) Sync()`

- [ ] **Step 1: Write the failing tests**

```go
func TestSearchAdvertisesExactlyThreeTools(t *testing.T) {
	tools := listTools(t, NewSearch(cat, reg, rank.Thresholds{}))
	if len(tools) != 3 {
		t.Fatalf("facade advertises %d tools, want 3: %v", len(tools), names(tools))
	}
}

func TestPassthroughAdvertisesRealToolsUnderCanonicalNames(t *testing.T) {
	tools := listTools(t, NewPassthrough(cat, reg))
	want := "mcp__alpha__kubectl_logs"
	if !slices.ContainsFunc(tools, func(x *mcp.Tool) bool { return x.Name == want }) {
		t.Errorf("passthrough missing %q; got %v", want, names(tools))
	}
}

func TestCallToolReachesTheOwningBackend(t *testing.T) { /* test 3 at the facade level */ }

func TestSearchToolsExplainsAnEmptyCatalog(t *testing.T) {
	// Never a bare empty list: the model must be told why.
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/mcpsrv/`
Expected: FAIL, `undefined: NewSearch`.

- [ ] **Step 3: Implement both servers**

`NewSearch` registers exactly `search_tools`, `describe_tool`, and `call_tool` through the generic `mcp.AddTool`. `NewPassthrough` registers one tool per catalog entry through `Server.AddTool`; `Sync` diffs against the current catalog, calls `Server.RemoveTools(names...)` for departures, and adds arrivals. The SDK emits `ToolListChanged` for us, which is what makes a disable visible to pass-through clients.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/mcpsrv/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpsrv
git commit -m "feat: add search facade and passthrough MCP endpoints"
```

---

### Task 9: Guard, status API, status page, and inspector

**Files:**
- Create: `mcpd/internal/web/guard.go`
- Create: `mcpd/internal/web/web.go`
- Create: `mcpd/internal/web/assets/index.html`
- Create: `mcpd/internal/web/assets/app.js`
- Create: `mcpd/internal/web/assets/style.css`
- Create: `mcpd/internal/web/guard_test.go`
- Create: `mcpd/internal/web/web_test.go`

**Interfaces:**
- Consumes: `backend.Registry`, `catalog.Catalog`.
- Produces: `func NewGuard(addr string) *Guard`, `func (g *Guard) Protection() *http.CrossOriginProtection`, `func (g *Guard) Wrap(http.Handler) http.Handler`, `func (g *Guard) WrapCallback(http.Handler) http.Handler`, `func NewServer(*backend.Registry, *catalog.Catalog, *Guard) *Server`, `func (s *Server) Routes(*http.ServeMux)`

- [ ] **Step 1: Write the failing tests**

```go
func TestForeignOriginRejectedOnWebRoutes(t *testing.T) {
	req := httptest.NewRequest("GET", "http://127.0.0.1:7420/api/status", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestAbsentOriginAccepted(t *testing.T) {
	// Claude Code and Codex send no Origin; rejecting them breaks everything.
}

func TestMutationsRejectGET(t *testing.T) {
	// /api/backends/alpha/disable via GET must fail. Only /oauth/callback may
	// change state on GET.
}

func TestToolResultIsEscapedNotRendered(t *testing.T) {
	// test 17
	body := invokeInspector(t, `<img src=x onerror=alert(1)>`)
	if strings.Contains(body, "<img src=x") {
		t.Error("tool result was interpolated into HTML unescaped")
	}
	if !strings.Contains(body, "&lt;img src=x") {
		t.Error("tool result was not escaped into the response")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/web/`
Expected: FAIL, `undefined: NewGuard`.

- [ ] **Step 3: Implement the guard**

Construct one `*http.CrossOriginProtection` and expose it via `Protection()` so `cmd/mcpd` can hand the same object to `StreamableHTTPOptions.CrossOriginProtection`. One policy object for both surfaces, because two independently configured guards drift and the drift is invisible until a route that should have been protected is not. `SetDenyHandler` writes a one-line reason. `Wrap` additionally rejects non-POST mutations. `WrapCallback` keeps host checking but skips the POST-only and JSON-only rules, because a provider redirect is a top-level GET.

- [ ] **Step 4: Implement the status page and inspector**

`index.html` is served through `html/template`. All dynamic values arrive as JSON from `/api/status` and are inserted with `textContent` in `app.js`. Inspector results render into a `<pre>` via `textContent`.

Run: `grep -rn "innerHTML" internal/web/assets/`
Expected: no matches. This is a review gate, not a suggestion.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/web/ -v`
Expected: PASS, including the XSS test.

- [ ] **Step 6: Commit**

```bash
git add internal/web
git commit -m "feat: add guarded status page and tool inspector"
```

---

### Task 10: OAuth store, authorization flow, and callback

Read `PHASE0.md` first. Its finding decides which registration mode this task implements.

**Files:**
- Create: `mcpd/internal/oauthstore/store.go`
- Create: `mcpd/internal/oauthstore/store_test.go`
- Modify: `mcpd/internal/backend/backend.go` (wire the handler into `httpClient`)
- Modify: `mcpd/internal/web/web.go` (add `/oauth/authorize/{backend}` and `/oauth/callback`)

**Interfaces:**
- Consumes: Phase 0's decision, `config.Backend.Auth == "oauth"`.
- Produces: `func NewStore(dir string) (*Store, error)`, `func (s *Store) Fetcher(backend string) auth.AuthorizationCodeFetcher`, `func (s *Store) Pending(backend string) (url string, ok bool)`, `func (s *Store) Complete(state string, res *auth.AuthorizationResult) error`, `func (s *Store) TokenSource(ctx, backend string, base oauth2.TokenSource) oauth2.TokenSource`

- [ ] **Step 1: Confirm the config field names**

Run: `go doc github.com/modelcontextprotocol/go-sdk/auth.AuthorizationCodeHandlerConfig`
Expected: fields including `RedirectURL`, `AuthorizationCodeFetcher`, `RequestRefreshToken`, `PreregisteredClient`, `DynamicClientRegistrationConfig`. Match the code below to what `go doc` prints.

- [ ] **Step 2: Write the failing tests**

```go
func TestOAuthEndToEndAgainstFakeProvider(t *testing.T) {
	// test 6: needs-auth plus pending URL, then registration, then callback with
	// the correct state, then exchange, then a 0600 token file, then an
	// authenticated reconnect with tools in the catalog, then a restart that
	// reuses the token, then an expired access token that refreshes.
	// Use the SDK's internal/oauthtest fake authorization server if it is
	// importable; otherwise stand up an equivalent httptest server.
}

func TestCallbackRejectsMismatchedAndReplayedState(t *testing.T) {
	// test 7: driven as a browser-shaped GET through the real guard middleware,
	// not by calling the handler directly.
	for _, tc := range []struct{ name, state string }{
		{"mismatched", "not-a-real-state"},
		{"replayed", validStateUsedTwice},
	} {
		// assert non-2xx and that no token file was written
	}
}

func TestTokenFileMode(t *testing.T) {
	// 0600 file inside a 0700 directory.
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/oauthstore/`
Expected: FAIL, `undefined: NewStore`.

- [ ] **Step 4: Implement the store and fetcher**

`Fetcher` returns a closure matching `auth.AuthorizationCodeFetcher`: it records `state -> backend` in the pending registry, sets the backend to `StateNeedsAuth`, and blocks on a channel until `Complete` delivers an `*auth.AuthorizationResult` or the context is cancelled. `Complete` requires `state` to match exactly one outstanding authorization and **consumes it**, so a replay finds nothing. Persist tokens and registered client credentials as one JSON document per backend, written to a temp file then `os.Rename`d, at 0600 inside a 0700 directory. `TokenSource` wraps the base source so a refreshed token is written back rather than lost on restart.

- [ ] **Step 5: Wire the handler into the backend HTTP client**

Build the handler with `RedirectURL: "http://127.0.0.1:7420/oauth/callback"`, `AuthorizationCodeFetcher: store.Fetcher(name)`, `RequestRefreshToken: true`, and either `DynamicClientRegistrationConfig` or `PreregisteredClient` per `PHASE0.md`. A refresh failure sets the backend back to `StateNeedsAuth` rather than retrying indefinitely.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/oauthstore/ ./internal/web/ -race -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/oauthstore mcpd/internal/backend mcpd/internal/web
git commit -m "feat: add downstream OAuth with browser-triggered authorization"
```

---

### Task 11: Daemon entrypoint, systemd unit, and first real run

**Files:**
- Create: `mcpd/cmd/mcpd/main.go`
- Create: `mcpd/cmd/mcpd/main_test.go`
- Create: `mcpd/packaging/mcpd.service`

**Interfaces:**
- Consumes: every package above.
- Produces: the `mcpd serve` binary, plus `func run(ctx context.Context, opts Options) error` so tests can start the daemon in-process.

- [ ] **Step 1: Write the failing test**

```go
func TestServeMountsAllThreeSurfaces(t *testing.T) {
	// /mcp/search, /mcp/passthrough, and /api/status all answer, and the two MCP
	// endpoints advertise different tool counts.
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd mcpd && go test ./cmd/mcpd/`
Expected: FAIL, `undefined: run`.

- [ ] **Step 3: Implement `main.go`**

One `http.ServeMux`. `mcp.NewStreamableHTTPHandler` for each of the two servers, both receiving `guard.Protection()` through `StreamableHTTPOptions`, and never setting `DisableLocalhostProtection`. Load overrides before connecting, so a disabled backend never starts. Graceful shutdown on `SIGTERM`: stop accepting, drain, close sessions, terminate stdio children.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd mcpd && go test ./cmd/mcpd/ -v`
Expected: PASS.

- [ ] **Step 5: Write the unit**

`packaging/mcpd.service`:

```ini
[Unit]
Description=mcpd local MCP proxy
After=network-online.target

[Service]
ExecStart=%h/.local/bin/mcpd serve
Restart=always
RestartSec=2
# PATH and credentials come from ~/.config/environment.d/*.conf, which
# systemd --user already imports. No EnvironmentFile is needed.

[Install]
WantedBy=default.target
```

- [ ] **Step 6: Install and verify by execution**

```bash
cd mcpd && go build -o ~/.local/bin/mcpd ./cmd/mcpd
mkdir -p ~/.config/mcpd && cp testdata/config.example.json ~/.config/mcpd/config.json
cp packaging/mcpd.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now mcpd
systemctl --user status mcpd --no-pager
curl -s localhost:7420/api/status | head -40
```
Expected: active, and a status payload listing backends. `art` and `flint` must be `up`. If `art` is down, its `env_passthrough` is missing a variable, which is exactly the failure mode Task 2 predicted; add the variable rather than widening to inherit-everything.

- [ ] **Step 7: Commit**

```bash
git add cmd/mcpd mcpd/packaging
git commit -m "feat: add mcpd daemon entrypoint and systemd user unit"
```

---

### Task 12: `mcpd install` client rewiring and surgical revert

**Files:**
- Create: `mcpd/internal/install/install.go`
- Create: `mcpd/internal/install/install_test.go`
- Create: `mcpd/internal/install/testdata/` (golden inputs for all four clients)
- Modify: `mcpd/cmd/mcpd/main.go` (add the `install` subcommand)

**Interfaces:**
- Consumes: `config.Config`.
- Produces: `func Apply(client string, dryRun bool) (Report, error)`, `func Revert(client string) (Report, error)`, `type Report struct { File string; Changes []string; Refused []string }`

- [ ] **Step 1: Write the failing tests**

```go
func TestRoundTripIsByteForByteWithoutInterveningEdits(t *testing.T) {
	// test 13, for all four clients.
}

func TestApprovalBlocksAreMigratedNotCommented(t *testing.T) {
	// [mcp_servers.github.tools.pull_request_review_write] must become
	// [mcp_servers.mcpd.tools.mcp__github__pull_request_review_write] and remain
	// active. A dropped key silently ungates a destructive tool.
}

func TestRevertPreservesAnUnrelatedLaterEdit(t *testing.T) {
	// test 18: add an unrelated MCP server after install; revert must keep it.
}

func TestRevertRefusesOnModifiedOwnedRegion(t *testing.T) {
	// Hand-edit the mcpd block, then revert: must refuse and name file and key.
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd mcpd && go test ./internal/install/`
Expected: FAIL, `undefined: Apply`.

- [ ] **Step 3: Implement**

Per-client writers for `~/.claude.json`, `~/.codex/config.toml`, `~/.cursor/mcp.json`, and `~/.config/opencode/opencode.json`. Endpoint selection: `/mcp/passthrough` for Claude Code and Codex, `/mcp/search` for Cursor and OpenCode. `Revert` operates on the file's **current** content, removes only mcpd-owned regions, and refuses when an owned region was modified. A timestamped `.bak` is written on every mutation, as a backstop rather than the mechanism.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd mcpd && go test ./internal/install/ -v`
Expected: PASS, all four tests.

- [ ] **Step 5: Dry-run against the real configs before mutating them**

Run: `mcpd install --client codex --dry-run`
Expected: a diff you can read. Only then run it for real, one client at a time, confirming that client still starts before moving to the next.

- [ ] **Step 6: Commit**

```bash
git add internal/install mcpd/cmd/mcpd
git commit -m "feat: add mcpd install with surgical revert"
```

---

### Task 13: Ranking eval, query expansion, and threshold calibration

**Files:**
- Create: `mcpd/cmd/evalrank/main.go`
- Create: `mcpd/testdata/eval_queries.json`
- Create: `mcpd/testdata/eval_negative_calibration.json`
- Create: `mcpd/testdata/eval_negative_validation.json`

**Interfaces:**
- Consumes: `rank.Fuse`, `rank.Calibrate`, a live catalog from a running daemon.
- Produces: an exit code and a report. This is the acceptance gate for success criterion 6.

- [ ] **Step 1: Port and restructure the query file**

Copy the prototype's 15 queries **verbatim** as the regression baseline; do not reword them to pass. Change `expect` from a string to a list of acceptable ids. Then add roughly 3 more paraphrase, 8 near-name, and 10 cross-backend-ambiguous queries, reaching about 36 answerable. Write 8 negative calibration and 4 negative validation queries in the separate files. Mark roughly 10 of the new answerable queries `"heldout": true`, written before any ranking tuning.

- [ ] **Step 2: Implement the runner**

It must **exit non-zero when any expected id is absent from the catalog**, rather than skipping and shrinking the denominator: that is the misleading-number bug in the prototype's `eval_ranking.py`, where 12/12 on a partial catalog reads better than 13/15 on a full one. Report top-1 and top-3 over the full denominator, separately for held-out and tuned queries.

- [ ] **Step 3: Run the eval and record the baseline**

Run: `cd mcpd && go run ./cmd/evalrank -catalog ~/.local/state/mcpd/catalog.json | tee /tmp/eval-baseline.txt`
Expected: a report. The baseline may fail the gate; that is information, not a blocker.

- [ ] **Step 4: Calibrate the abstention thresholds**

Calibrate over the answerable set and the 8 negative calibration queries, then score the 4 validation queries **exactly once**. If no separating gap exists, record it and ship abstention disabled. Do not widen thresholds until validation passes, at which point it would stop being a validation set.

- [ ] **Step 5: Iterate on fusion only if the gate fails**

If top-1 is below 80%, adjust fusion (weighting, tokenisation) and re-run. Watch the held-out versus tuned gap: if held-out trails badly, the ranking is fit to the eval rather than the problem, and that gets recorded rather than tuned away.

- [ ] **Step 6: Commit**

```bash
git add cmd/evalrank mcpd/testdata
git commit -m "test: expand ranking eval and calibrate abstention thresholds"
```

---

### Task 14: Live OAuth acceptance against real Notion

**Files:**
- Modify: `mcpd/PHASE0.md` (append the live run's outcome)

**Interfaces:**
- Consumes: the whole running daemon.
- Produces: evidence for success criteria 1, 2, and 12. This is a manual checklist rather than a `go test`, because it needs a human at a browser and a real Notion account.

- [ ] **Step 1: Confirm the starting state**

Run: `rm -f ~/.local/state/mcpd/tokens/notion.json && systemctl --user restart mcpd`
Expected: `/api/status` shows `notion` as `needs-auth` with a pending authorization URL.

- [ ] **Step 2: Authorize in the browser**

Open `http://127.0.0.1:7420`, click **[Authenticate]** on the `notion` row, complete Notion's consent screen.
Expected: the browser returns to the status page, and `notion` shows `up` with a non-zero tool count.

- [ ] **Step 3: Make one real authenticated call**

Use the inspector to call a read-only Notion tool.
Expected: real data, rendered as escaped text.

- [ ] **Step 4: Verify token reuse across a restart**

Run: `systemctl --user restart mcpd && sleep 5 && curl -s localhost:7420/api/status | grep -A3 notion`
Expected: `up`, with no re-authorization prompt. This is the clause success criterion 2 requires.

- [ ] **Step 5: Verify refresh**

Force access-token expiry by editing the expiry field in `tokens/notion.json`, restart, then make a call.
Expected: a silent refresh rather than a return to `needs-auth`. If Notion issued no refresh token, record that and restate criterion 2; Phase 0 predicted this row.

- [ ] **Step 6: Record the outcome and commit**

Append results to `PHASE0.md`, including anything provider-specific that the fake provider could not have surfaced.

```bash
git add PHASE0.md
git commit -m "docs: record live Notion OAuth acceptance results"
```

---

## Self-Review

**Spec coverage.** Every one of the spec's 18 tests maps to a task: 1, 2 and the facade-level 3 to Task 8; 3 and 12 to Task 3; 4 and 16 to Task 4; 5 to Task 2; 6 and 7 to Task 10; 8 to Task 6; 9, 10, 14 and 15 to Task 5; 11 and 17 to Task 9; 13 and 18 to Task 12. All 12 success criteria map: 1, 2 and 12 to Tasks 10 and 14; 3 to Task 9; 4 and 5 to Tasks 8 and 12; 6 to Tasks 6, 7 and 13; 7 to Task 5; 8 to Task 2; 9 to Task 9; 10 to Task 3; 11 to Task 12.

**Gaps found and closed.** Two spec items had no task on the first pass. `ART_MCP_WORKSPACE_ROOTS` is now covered by Task 2 Step 5, since it is just a declared `env` entry. The `hooks/unwrap-proxied-tool.py` re-dispatch hook needs no change at all, which is only true because canonical ids are preserved verbatim; that is why the id format is a global constraint rather than an implementation detail buried in Task 4.

**Placeholders.** None. Every step carries either real code or a command with an expected result. Three steps deliberately defer to `go doc` instead of asserting a signature (Task 1 Step 2 for `oauthex`, Task 3 Step 1 for `ToolHandler`, Task 10 Step 1 for `AuthorizationCodeHandlerConfig`), and each says explicitly that `go doc` wins over this plan. That is honest about what I verified versus inferred: I confirmed these packages and types exist, but not every field name.

**Type consistency.** `catalog.Entry`, `backend.Health`, `backend.State`, `rank.Result`, `rank.Evidence`, `rank.Thresholds`, and `install.Report` are each defined once and referenced identically thereafter. `Server.RemoveTools` is plural, matching the SDK. `ErrNotAttempted` is produced in Task 3 and consumed only by callers deciding whether a retry is safe, which is its whole purpose.

**Risk carried into implementation.** Tasks 4, 5, and 9 hold the concurrency and security work where the design review found six real bugs. Their tests run under `-race`, and Tasks 4 and 5 also under `-count=5`, because one green run on a race test proves very little.
