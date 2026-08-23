# mcp-endpoints Specification

## Purpose
Define the search and pass-through MCP endpoints, their client orientation, native tool-search selection, backend routing, and catalog-change notifications.

## Requirements

### Requirement: Both endpoints tell the model what they are for

Each endpoint SHALL send MCP `instructions` at initialize, stating how a tool is reached and
naming the backends that are declared.

Instructions are the only channel by which a server states its own purpose, and a client puts
them in front of the model directly. Their absence is not cosmetic and it is not recoverable
from the tool list. On the facade the model sees three generic verbs that give no hint that
several hundred tools sit behind them. On pass-through it sees prefixed names that do not say
these backends are reachable through nothing else. Naming the declared backends is what tells
the model a domain is present at all, which it needs before it thinks to search.

This was observed rather than anticipated: with every tool listed and every description intact,
an agent still did not use them, because nothing told it they were there to be used. The
prototype this daemon superseded carried such text, so shipping without it was a regression.

#### Scenario: A client receives usable orientation from either endpoint

- **WHEN** a client completes initialize against either endpoint
- **THEN** the instructions are non-empty, say how to reach a tool, and name the declared
  backends

### Requirement: Two endpoints on distinct paths

The daemon SHALL expose two MCP endpoints over streamable HTTP at distinct paths:
`/mcp/search` advertising the three-tool facade, and `/mcp/passthrough` advertising every
catalog tool under its canonical id. Mode SHALL be selected by path rather than by query
parameter, because some clients normalise or drop query strings on MCP URLs.

Both endpoints SHALL share the same sessions, catalog, and token store.

#### Scenario: The passthrough endpoint advertises real tools

- **WHEN** a client lists tools on the passthrough endpoint
- **THEN** it receives one tool per catalog entry, named `mcp__<server>__<tool>`

#### Scenario: A call through either endpoint reaches the owning backend

- **WHEN** a tool is invoked through either endpoint
- **THEN** the call is dispatched to the backend that owns that tool and its result is
  returned unchanged

### Requirement: Clients with native tool search receive pass-through

Clients that implement their own tool search SHALL be pointed at `/mcp/passthrough`, so
their native layer ranks against real schemas. Placing the three-tool facade behind such
a client stacks two search layers: the native layer indexes `search_tools` and must then
guess a query to hand to the proxy's search, which ranks worse than either layer alone
and was measured to save no tokens.

#### Scenario: A native-search client sees real schemas

- **WHEN** Claude Code or Codex connects
- **THEN** it is served the full catalog and its own tool search operates unchanged

#### Scenario: A client without native search sees the facade

- **WHEN** Cursor CLI or OpenCode connects
- **THEN** it is served three tool schemas rather than the full catalog

### Requirement: Catalog changes are announced

When the catalog changes, the daemon SHALL emit a tool-list-changed notification on both
endpoints so pass-through clients re-list. The facade's own three tools never change, so
facade clients require no notification and simply observe the new catalog on their next
search.

#### Scenario: Disabling a backend updates connected pass-through clients

- **WHEN** a backend is disabled while a pass-through client is connected
- **THEN** a tool-list-changed notification is emitted and the client's next listing omits
  that backend's tools
