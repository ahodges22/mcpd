# tool-catalog Specification

## Purpose
Define canonical tool identities and a resilient, persistent catalog with coalesced refreshes and per-backend commit granularity.

## Requirements

### Requirement: Canonical tool identifiers

Every tool SHALL be addressed by the identifier `mcp__<server>__<tool>`. This format
SHALL NOT change, because existing agent permission hooks match on the inner tool name
and re-dispatch proxied calls to those hooks by that name.

#### Scenario: A tool is addressable by canonical id

- **WHEN** the backend `github` reports a tool named `create_pull_request`
- **THEN** the catalog exposes it as `mcp__github__create_pull_request`

### Requirement: A dead backend does not sink the catalog

A backend that fails to list its tools SHALL be excluded from the catalog and recorded
with its error, and every other backend's tools SHALL still be served.

#### Scenario: One backend fails during a full refresh

- **WHEN** one of two backends fails to list and the other succeeds
- **THEN** the catalog contains the successful backend's tools, and search still answers

### Requirement: Refresh coalescing with a trigger counter

Refresh has four independent triggers: TTL expiry, a manual re-index, an upstream
tool-list-changed notification, and a backend reconnect. At most one `tools/list` per
backend SHALL be in flight. A trigger arriving during an active refresh SHALL cause
exactly one follow-up read after the current one completes, rather than being satisfied
by the read already running.

The daemon SHALL track a trigger counter per backend, record its value when a refresh
starts, and read again if the counter has advanced on completion. The loop SHALL end
when a read completes with the counter unchanged.

#### Scenario: A notification is not satisfied by an older read

- **WHEN** a tool-list-changed notification arrives while a `tools/list` is in flight
- **THEN** a second read is issued after the first completes, and the second result is
  the one committed

#### Scenario: The loop converges rather than stopping at a fixed count

- **WHEN** a further trigger arrives during that second read
- **THEN** a third read is issued, because the guarantee is a read strictly after the
  most recent trigger and not a fixed maximum number of reads

#### Scenario: A burst of triggers collapses

- **WHEN** several triggers arrive within the debounce window
- **THEN** exactly one follow-up read is issued rather than one per trigger

#### Scenario: A pathological backend degrades rather than spins

- **WHEN** a backend emits tool-list-changed continuously
- **THEN** consecutive refresh rounds back off up to the TTL, so the daemon polls at the
  cap instead of spinning

### Requirement: Per-backend commit granularity

A full re-index SHALL be a fan-out of independent per-backend refreshes rather than one
transaction. One slow or failing backend SHALL NOT delay or discard another's results,
and each backend SHALL be either fully updated or untouched, never half-merged.

#### Scenario: A slow backend does not block a fast one

- **WHEN** a full re-index runs and one backend is slow to respond
- **THEN** the other backends' results are committed without waiting for it

### Requirement: Catalog persistence

The catalog SHALL be persisted under the daemon's state directory so that a restart can
serve search before every backend has been re-listed.

#### Scenario: The catalog survives a restart

- **WHEN** the daemon restarts with a previously written catalog
- **THEN** search answers from the persisted catalog while backends reconnect
