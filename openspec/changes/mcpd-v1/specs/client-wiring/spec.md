## ADDED Requirements

### Requirement: One command rewires every client

The daemon SHALL provide a subcommand that points a named client at the appropriate
endpoint, choosing pass-through for clients with native tool search and the facade for
clients without. Existing direct backend declarations SHALL be stashed rather than
deleted, and a timestamped backup SHALL be written on every mutation.

#### Scenario: A client is rewired to the correct endpoint

- **WHEN** the install subcommand runs for a client with native tool search
- **THEN** that client's configuration points at the pass-through endpoint

#### Scenario: Changes can be previewed before being applied

- **WHEN** the install subcommand is run in dry-run mode
- **THEN** it reports what it would change and modifies nothing

### Requirement: Per-tool approval settings are migrated, not discarded

Where a client supports per-tool approval settings, those settings SHALL be rewritten to
the new server and tool names rather than commented out or dropped.

This is the highest-consequence behaviour in this capability and it fails silently: a
dropped or mistyped key leaves the configuration valid and the daemon working, while a
destructive tool quietly loses its approval gate, and the only symptom is the absence of a
prompt the user was not expecting.

Clients served the facade SHALL be documented as losing per-tool approval granularity,
because such a client sees only the facade's `call_tool` and therefore has one
undifferentiated approval decision for every upstream tool.

#### Scenario: An approval gate survives rewiring

- **WHEN** a client declares an approval requirement for a specific upstream tool and is
  rewired to pass-through
- **THEN** the requirement is present under the new server and tool name, and is active
  rather than commented out

### Requirement: Revert removes only what was added

Revert SHALL operate on the target file's current content and SHALL remove only entries
the tool itself created, preserving everything else. It SHALL NOT restore a whole-file
snapshot, because between installing and reverting the user may have made unrelated edits
that a snapshot restore would silently destroy.

Where a region the tool owns has been modified by hand since installation, revert SHALL
refuse and name the file and key rather than guessing whose version wins, because refusing
is recoverable and a silent wrong merge is not.

#### Scenario: An unrelated later edit survives revert

- **WHEN** the user adds an unrelated MCP server to a client's configuration after
  installing, then reverts
- **THEN** the unrelated server is still present and only the tool's own entries are gone

#### Scenario: With no intervening edits, revert is exact

- **WHEN** install then revert run with no edits in between
- **THEN** the file is byte-for-byte identical to its original content

#### Scenario: A hand-modified owned region blocks revert

- **WHEN** the user has hand-edited an entry the tool created, then reverts
- **THEN** revert refuses and reports which file and key conflict

### Requirement: Committed example configuration carries no internal hostnames

Any example configuration committed to the repository SHALL use placeholder hostnames. The
working configuration lives outside version control, so committed examples need only
demonstrate shape.

#### Scenario: The example config is safe to publish

- **WHEN** the committed example configuration is inspected
- **THEN** it contains no internal infrastructure hostnames
