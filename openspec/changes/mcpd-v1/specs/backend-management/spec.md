## ADDED Requirements

### Requirement: Backends can be declared and removed from the status surface

The status surface SHALL let the user declare a new backend and remove an existing one,
for both the stdio and the HTTP transport, without editing a file by hand and without
restarting the daemon. Adding a backend was the fourth of the four pains this change
exists to fix, and it is the only one that still required a text editor.

Editing an existing declaration is out of scope. A declaration can carry an inline
credential in `env` or `headers`, so an edit form would have to send that credential back
to the browser to prefill itself. Remove and re-add is the supported path, and it never
sends a stored value outward.

#### Scenario: An added backend serves tools without a restart

- **WHEN** the user declares a backend from the status surface
- **THEN** the daemon connects it, lists its tools, and the catalog answers a search for
  them, with no restart and without disturbing any other backend's session

#### Scenario: A removed backend stops serving

- **WHEN** the user removes a backend
- **THEN** its session closes, a stdio child terminates, its tools leave the catalog, and
  its name is no longer routable

#### Scenario: A rejected declaration changes nothing

- **WHEN** a declaration is invalid, for example because it names neither a command nor a
  URL, or names both
- **THEN** it is refused, the configuration file is unchanged, and no backend is
  registered

### Requirement: The daemon owns the declaration file write path

The daemon SHALL write the user's configuration file when the user adds or removes a
backend. This reverses the earlier decision in this change that the file was read-only to
the daemon.

The reversal is deliberate and costs the safest property the earlier design had. It is
taken because the alternative, a second daemon-owned overlay file, makes the effective set
of backends the union of two files. A reader then has to merge them by hand to answer
"what is declared", and the file the user edits stops being the answer. One file remains
the single source of truth for declarations, and the requirements below exist to make
writing it safe.

Enable and disable SHALL remain in daemon state and SHALL NOT be written to the
configuration file, because a disable is a runtime override rather than a declaration.

#### Scenario: A write preserves what the daemon does not model

- **WHEN** the daemon rewrites the file to add or remove a backend
- **THEN** every other backend's declaration is preserved, including any field the
  daemon's own types do not model, because the daemon rewrites only the entry it changed

#### Scenario: Toggling a backend still does not write the file

- **WHEN** a backend is disabled from the status surface
- **THEN** the override is recorded in daemon state and the configuration file is unchanged

### Requirement: A hand edit is never lost to a daemon write

The file is hand-authored, so the user and the daemon are two writers with no lock between
them. A write SHALL be refused rather than applied when the file on disk no longer matches
what the daemon last read. The daemon SHALL keep a copy of the previous content beside the
file, so a write the user did not want is recoverable.

Refusing is correct rather than merging: the daemon cannot tell an intentional hand edit
from a stale buffer about to be saved, and a silent merge loses one of them.

#### Scenario: A write is refused when the file changed underneath it

- **WHEN** the file has been edited by hand since the daemon read it, and the user then
  adds a backend from the status surface
- **THEN** the write is refused, the file is unchanged, and the surface reports that the
  file changed on disk

#### Scenario: The previous content is recoverable

- **WHEN** the daemon has written the file
- **THEN** the content it replaced is readable from a copy beside it

### Requirement: Declarations can be reloaded without a restart

The daemon SHALL reload the declaration file on request. Reload is what adopts a hand edit,
and it is what clears the refusal above without a restart.

A reload SHALL leave an unchanged backend untouched. Rebuilding every backend would drop
live sessions, terminate healthy stdio children, and force an OAuth backend through a
handshake it did not need.

#### Scenario: A hand-added backend appears on reload

- **WHEN** the user adds a backend by editing the file and then triggers a reload
- **THEN** the new backend is registered and its tools are listed

#### Scenario: A hand-removed backend is torn down on reload

- **WHEN** a declaration is deleted from the file and a reload is triggered
- **THEN** that backend's session closes and its tools leave the catalog

#### Scenario: An unchanged backend survives a reload

- **WHEN** a reload adds or removes one backend
- **THEN** every other backend keeps its session, its stdio child, and its authorization

### Requirement: A backend name is validated before anything trusts it

A backend name becomes a URL path segment, a file name under the state directory, and the
prefix of every canonical tool id for that backend. Until now names could only come from a
file the user wrote, so the daemon trusted them. A name arriving over HTTP SHALL be
validated, and the same validation SHALL apply on the file path, because one validator that
both paths share cannot drift.

#### Scenario: A name that could escape the state directory is refused

- **WHEN** a declaration names a backend using a path separator or a parent-directory
  reference
- **THEN** it is refused, and no file is written inside or outside the state directory

#### Scenario: A duplicate name is refused

- **WHEN** a declaration reuses the name of a backend that is already declared
- **THEN** it is refused rather than replacing the existing declaration

### Requirement: Removal is confirmed and leaves no orphaned state

Removal deletes a declaration the user may have written by hand, so the surface SHALL
require a second confirming action, as it already does for a destructive tool. As with that
one, the confirmation protects against a misclick and is not a security control.

A removal SHALL delete the removed backend's runtime state: its enable or disable override,
and its stored OAuth record. State left behind under a name that is no longer declared
would silently apply to a later backend that reused the name.

#### Scenario: Removal requires a second confirming action

- **WHEN** the user triggers a removal from the status surface
- **THEN** the surface requires a second confirming action before the request is sent

#### Scenario: A removed backend leaves no stored token

- **WHEN** an authorized OAuth backend is removed
- **THEN** its stored token record is deleted, and re-adding the same name requires a fresh
  authorization

### Requirement: Writing the file does not widen access to a credential

A declaration can carry an inline credential in `env` or `headers`. The daemon SHALL NOT
leave the file readable beyond its owner after writing it.

#### Scenario: A permissive mode is tightened on write

- **WHEN** the daemon writes a configuration file that is readable beyond its owner
- **THEN** the written file is readable by its owner only
