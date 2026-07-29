# mcpd

A local MCP proxy: one daemon fronting many MCP backends for every coding agent on
this machine, with built-in tool search, a status UI, and browser-triggered OAuth.

Status: **in development.** Phase 0 (OAuth feasibility) is complete; see
[`PHASE0.md`](PHASE0.md). No daemon yet.

## Why

Four local agents (Claude Code, Codex, Cursor CLI, OpenCode, plus Zed over ACP) each
maintain their own MCP configuration against the same backends. That produces four
problems at once:

1. **Token bloat in clients without native tool search.** Cursor CLI and OpenCode
   load every tool schema up front. Claude Code and Codex have native tool search and
   do not.
2. **OAuth-gated backends are unusable through a proxy** without an OAuth client.
3. **No visibility when a backend dies.** Failures are silent, and over ACP there is
   no MCP status surface at all.
4. **Config sprawl.** Adding a backend means editing four files.

## Shape

```
                    +------------------------------ mcpd (systemd --user) ---+
claude code --+     |                                                        |
codex       --+---> |  /mcp/passthrough  -+                                  |
              |     |                     +-> session manager --> backends   |
cursor cli  --+---> |  /mcp/search       -+        |                         |
opencode    --+     |                              +- catalog + embeddings   |
zed (acp)   --+     |  /            web UI  -------+                         |
                    |  /oauth/callback      -------+- token store            |
                    +--------------------------------------------------------+
                       127.0.0.1:7420 (loopback only)
```

Clients that already have native tool search get `/mcp/passthrough`, so their own
search ranks against real schemas. Clients that do not get `/mcp/search`, a
three-tool facade (`search_tools`, `describe_tool`, `call_tool`). Stacking two
search layers ranks worse than either alone, which is why the mode is per client.

## Design

The full design lives outside this repo, in the Obsidian vault:
`_plans/art-agent-scratch/2026-07-28-mcpd-local-mcp-proxy-design.md`, with the
implementation plan alongside it. Both went through ten rounds of adversarial review.

Load-bearing decisions, in case the spec is not to hand:

- **At-most-once `tools/call`.** Reconnect only when no send was attempted. A write
  that returns an error is not evidence the upstream did not act, so it is never
  replayed. The catalog includes tools that mutate infrastructure and open pull
  requests.
- **Least-privilege child environments.** Every stdio child gets a constructed
  `cmd.Env`: a curated base, plus that backend's declared `env`, plus its declared
  `env_passthrough`. Never `nil`, which would inherit every credential the daemon
  holds and hand them to third-party npm code.
- **Origin-guarded loopback.** DNS-rebinding protection stays on, and one
  `http.CrossOriginProtection` is shared between the MCP endpoints and the web routes
  so the two cannot drift apart.
- **Config is yours.** `~/.config/mcpd/config.json` is written by you and never by
  the daemon. Runtime state lives under `~/.local/state/mcpd/`.

## Security posture

State it plainly, because the mitigations only make sense against it.

- **Local processes are trusted.** mcpd has no user authentication. Any process
  running as this user can reach the daemon and can therefore call any tool any
  backend offers. This is not equivalent to the situation before mcpd existed:
  introducing an HTTP surface where there was none is a real increase in exposure,
  and everything below exists because of it.
- **A hostile stdio backend is not contained.** It runs as the same user, so it can
  read the token store and every other credential file this account owns. The
  constructed `cmd.Env` addresses accidental credential grants and makes what a
  backend was given auditable. It is not a sandbox.
- **Browser-originated requests are the part that is actually mitigated.** The
  listener binds loopback only. One `http.CrossOriginProtection` is shared by the MCP
  endpoints and the web routes, so a page in an open tab on another origin is rejected
  on both. That check reads only `Sec-Fetch-Site` and `Origin`, and neither survives
  DNS rebinding: once the attacker's name resolves to 127.0.0.1 the browser believes
  it is same-origin. Rebinding is stopped by a separate `Host` header check instead,
  which must name a loopback address. The MCP endpoints get theirs from the SDK's own
  handler, which is why `DisableLocalhostProtection` is left alone; the web routes get
  theirs from the guard. Every state change requires a POST with a JSON content type,
  which a cross-origin form submission cannot produce, so no state change is reachable
  by navigation or an image load. The OAuth callback is the single exemption, being
  necessarily a top-level GET, and is protected by its one-time `state` nonce instead.
  Pages carry `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, so
  the one-click actions cannot be clickjacked from a frame. All backend-derived text is
  escaped by `html/template` or inserted with `textContent`, and a test walks the
  embedded assets to keep it that way.

## Build

```bash
go build ./...
go test ./... -race
```

Requires Go 1.25 or later, for `http.CrossOriginProtection`.

## Phase 0 probe

```bash
go run ./cmd/oauthprobe -server https://mcp.notion.com/mcp
go run ./cmd/oauthprobe -server https://mcp.notion.com/mcp -register  # creates a client
```

Answers whether a server supports metadata discovery, Dynamic Client Registration,
and a plain-HTTP loopback redirect URI. Useful for any new OAuth-gated backend, not
just Notion.
