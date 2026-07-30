## Why

Four coding agents on this machine (Claude Code, Codex, Cursor CLI, OpenCode, plus Zed
driving Claude and Codex over ACP) each maintain their own MCP configuration against
the same 13 backends. That produces four concrete pains, observed rather than
anticipated:

- Cursor CLI and OpenCode load every tool schema up front, roughly 583 tools. Claude
  Code and Codex have native tool search and do not.
- Notion cannot be proxied at all today, because it needs interactive OAuth with no
  static token, so it is configured direct in each client or not at all. `metabase` has
  never authenticated anywhere.
- Backends fail silently. A dead backend is skipped, and over ACP there is no MCP
  status surface of any kind, so "is the tool missing or is the server down?" is
  unanswerable.
- Adding one backend means editing `~/.claude.json`, `~/.codex/config.toml`,
  `~/.cursor/mcp.json`, and `~/.config/opencode/opencode.json`.

An existing Python prototype (`mcp-tool-search/`, 758 LOC, in the `art-agent-scratch`
repo) addresses only the first, and its own README names the other two as known gaps.

## What Changes

- A new Go daemon, `mcpd`, run under `systemd --user` on `127.0.0.1:7420`, holding one
  session per backend so stdio children are spawned once rather than once per client.
- Two MCP endpoints over streamable HTTP: `/mcp/search` exposing a three-tool facade,
  and `/mcp/passthrough` exposing the full catalog under `mcp__<server>__<tool>` names.
  The endpoint is chosen per client, because putting a facade behind a client that
  already has native tool search ranks worse than either layer alone.
- Hybrid tool ranking: the prototype's lexical scorer fused with LiteLLM embeddings by
  reciprocal rank, plus a separate abstention signal so the facade can say "no good
  match" instead of always returning three plausible-looking tools.
- Downstream OAuth to upstream MCP servers, triggered from a browser and persisted at
  mode 0600, so Notion works through the proxy for the first time.
- A loopback web UI: per-backend status, enable/disable, reconnect, re-index,
  authenticate, and a tool inspector.
- Adding and removing a backend from that UI, for both transports, so the fourth pain
  above no longer needs a text editor. This makes the declaration file daemon-writable,
  which reverses an earlier decision in this change and is why `backend-management`
  carries requirements for protecting a concurrent hand edit.
- `mcpd install` to rewire all four clients from one place, with a surgical `--revert`
  that removes only mcpd-owned entries.
- **BREAKING for the existing setup:** the `mcp-tool-search` prototype is superseded.
  Clients are rewired to `mcpd`, and Codex per-tool `approval_mode` blocks must be
  migrated to the new server and tool names or destructive GitHub tools silently lose
  their approval gate.

## Capabilities

### New Capabilities

- `backend-sessions`: upstream session lifecycle, at-most-once dispatch, least-privilege
  child environments, and the enable/disable kill switch.
- `tool-catalog`: flattening tools to canonical ids, persistence, and coalescing refresh
  across four independent trigger sources.
- `tool-search`: the three-tool facade, reciprocal rank fusion, and abstention.
- `mcp-endpoints`: the two streamable-HTTP endpoints and per-client mode selection.
- `backend-oauth`: downstream OAuth as a client, including registration, the
  browser-triggered authorization flow, token persistence, and refresh.
- `loopback-security`: the trust boundary, origin guarding, and escaping of
  backend-derived output.
- `status-ui`: the status page, the tool inspector, and their guardrails.
- `client-wiring`: rewiring the four clients, migrating approval blocks, and reverting
  without destroying unrelated user edits.
- `backend-management`: declaring and removing a backend at run time, the daemon's write
  path into the declaration file, reload, and name validation.

### Modified Capabilities

None. This is the first change in this repo; `openspec/specs/` is empty.

## Impact

- **New repo:** `ahodges22/mcpd` (private), at `~/Articulate/repos/mcpd`.
- **Supersedes:** `art-agent-scratch/mcp-tool-search/` (left in place and running until
  Task 12 rewires clients; not deleted by this change).
- **Mutates user config:** `~/.claude.json`, `~/.codex/config.toml`,
  `~/.cursor/mcp.json`, `~/.config/opencode/opencode.json`. This is the highest-risk
  surface in the change, because a bug there damages live configuration rather than the
  proxy.
- **New system units:** `~/.config/systemd/user/mcpd.service`.
- **New state:** `~/.config/mcpd/config.json` (hand-edited, and written by the daemon when
  a backend is added or removed) and `~/.local/state/mcpd/` (daemon-owned, including OAuth
  tokens at 0600).
- **Second writer on a hand-authored file:** the user and the daemon both write
  `config.json`, with no lock between them. Mitigated by refusing a write when the file
  changed on disk, and by keeping every version the daemon displaces in a numbered archive
  beside it. Seven adversarial review rounds went into `backend-management` for this reason;
  roughly half of that capability is machinery for sharing one file with a text editor, which
  is the price of the single-file choice over a daemon-owned overlay.
- **Name-keyed state is bound to its declaration:** a stored OAuth token or a disable override
  now records the declaration it was written under, and is inert under any other. Without it a
  repointed backend would present a token to an endpoint it was never issued for.
- **External dependency:** the LiteLLM gateway for embeddings at catalog-refresh time.
  Verified reachable, 1536 dimensions. Off-VPN with a warm cache stays fully
  functional.
- **Single point of failure introduced:** four independent partial failures become one
  total one. If the daemon is down, every client loses every backend. Accepted
  deliberately, mitigated with `Restart=always`.
- **Out of scope:** OS-level sandboxing of stdio backends, per-tool enable/disable,
  editing an existing backend declaration from the UI (remove and re-add instead, so no
  stored credential is ever sent to the browser), making a backend's reachability a
  precondition for declaring it, a per-declaration generation counter, user authentication on
  the daemon, forwarding sampling/elicitation/roots, and `metabase`.
