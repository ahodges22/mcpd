# backend-management Specification

## Purpose
Define safe runtime declaration, removal, reload, persistence, and reconciliation of MCP backends without restarting the daemon or corrupting configuration and state.

## Requirements

### Requirement: Backends can be declared and removed from the status surface

The status surface SHALL let the user declare a new backend and remove an existing one, for
both the stdio and the HTTP transport, without editing a file by hand and without restarting
the daemon. Adding a backend was the fourth of the four pains this change exists to fix, and
it is the only one that still required a text editor.

Editing an existing declaration is out of scope. A declaration can carry an inline credential
in `env` or `headers`, so an edit form would have to send that credential back to the browser
to prefill itself. Remove and re-add is the supported path, and it never sends a stored value
outward.

The declaration path SHALL be resolved through symlinks once at startup, and every write SHALL
use the resolved path. A configuration file symlinked into a dotfiles repository is a normal
setup, and an atomic exchange performed on the link would swap the link itself for a regular
file: the daemon would then read a detached copy while the user kept editing the original, and a
mode change would follow the link to a file the daemon was never asked to touch. Resolving once
keeps the link intact and keeps both sides looking at the same file. The archives and the
directory mode below then apply to the resolved location.

Removing the last remaining backend SHALL be permitted, and an empty backend set SHALL load.
Loading currently treats an empty set as a configuration error, which was reasonable while the
file could only be hand-written, and which this capability turns into a trap: the removal would
commit an empty set and the next start would then fail to load its own file. No requirement
depends on the set being non-empty, so the check goes rather than the feature.

#### Scenario: An added backend serves tools without a restart

- **WHEN** the user declares a backend from the status surface
- **THEN** the daemon connects it, lists its tools, and the catalog answers a search for them,
  with no restart and without disturbing any other backend's session

#### Scenario: A removed backend stops serving

- **WHEN** the user removes a backend
- **THEN** its session closes, a stdio child terminates, its tools leave the catalog, and its
  name is no longer routable

#### Scenario: Removing the last backend leaves a loadable file

- **WHEN** the only declared backend is removed
- **THEN** the write commits an empty backend set, and the daemon starts from that file rather
  than failing to load it

#### Scenario: A rejected declaration changes nothing

- **WHEN** a declaration is invalid, for example because it names neither a command nor a URL,
  or names both
- **THEN** it is refused, the configuration file is unchanged, no backend is registered, and no
  name-keyed state is created or deleted

### Requirement: The daemon owns the declaration file write path

The daemon SHALL write the user's configuration file when the user adds or removes a backend.
This reverses the earlier decision in this change that the file was read-only to the daemon.

The reversal is deliberate and costs the safest property the earlier design had. It is taken
because the alternative, a second daemon-owned overlay file, makes the effective set of
backends the union of two files. A reader then has to merge them by hand to answer "what is
declared", and the file the user edits stops being the answer. One file remains the single
source of truth for declarations, and the requirements below exist to make writing it safe.

Enable and disable SHALL remain in daemon state and SHALL NOT be written to the configuration
file, because a disable is a runtime override rather than a declaration.

#### Scenario: A write preserves what the daemon does not model

- **WHEN** the daemon rewrites the file to add or remove a backend
- **THEN** every other backend's declaration is preserved, including any field the daemon's own
  types do not model, because the daemon rewrites only the entry it changed

#### Scenario: Toggling a backend still does not write the file

- **WHEN** a backend is disabled from the status surface
- **THEN** the override is recorded in daemon state and the configuration file is unchanged

### Requirement: A competing hand edit is detected when visible, and an unexpected displacement is never destroyed

The user and the daemon are two writers on the same file with no lock between them. The daemon
SHALL serialize its own writes, SHALL refuse a write when the file on disk no longer matches
what it last read, and SHALL retain the versions it displaces, without limit for the one class
that is irreplaceable.

Prevention is not available and is not claimed. Advisory locking only binds writers that ask
for it, and the editors in use here do not, so a lock would buy a false sense of exclusion.
What is available is detection of every edit the daemon can see, plus a guarantee that a
displacement the daemon did not expect is always recoverable.

Five mechanisms carry this, and all five are load-bearing:

- **One writer inside the daemon.** Every daemon write, from either route and from reload, is
  serialized by a single mutex, so two of the daemon's own writes cannot interleave.
- **The baseline advances on success.** After a successful write the daemon adopts the bytes it
  just wrote as its new baseline, inside the same critical section. Without this the first
  write poisons every later one into a permanent false refusal.
- **The displaced version is captured atomically, not copied beforehand.** The write exchanges
  the temporary file and the configuration file in one atomic operation, so whatever content
  was in place at that instant ends up at the temporary path and is archived from there.
  Copying the old content before the exchange would miss an edit that landed between the copy
  and the swap, which is precisely the window that matters, so a copy-then-rename write cannot
  make this claim at all.
- **An unavailable exchange fails the write.** If the atomic exchange is unsupported, the
  daemon SHALL refuse the write and change nothing, rather than falling back to a plain rename.
  A fallback would silently reintroduce the destruction window the exchange exists to close,
  and it would do so exactly when nobody is watching.
- **Retention is bounded for routine versions and unbounded for surprising ones.** Each write
  archives the version it displaced beside the configuration. When the displaced content
  matches the baseline, the daemon displaced its own last write, and that archive is routine:
  the ten most recent routine archives are retained. When the displaced content does not match
  the baseline, the daemon displaced something it did not write, which is the only
  irreplaceable class, and that archive is retained without limit. A flat cap would let ten
  routine writes delete the single archive that mattered, and unbounded retention of every
  version would accumulate copies of a file that can hold inline credentials.

#### Scenario: A write is refused when the file changed underneath it

- **WHEN** the file has been edited by hand since the daemon read it, and the user then adds a
  backend from the status surface
- **THEN** the write is refused, the file is byte identical, and the surface reports that the
  file changed on disk

#### Scenario: A second write is not refused

- **WHEN** one write has already succeeded and another is attempted with no intervening edit
- **THEN** it is not refused as stale, because the daemon adopted the bytes it wrote as its new
  baseline

#### Scenario: An edit landing after the comparison is archived rather than lost

- **WHEN** the file is edited after the daemon's staleness comparison and before its commit
- **THEN** the edited content is readable from an archive, and it is still readable after ten
  later routine writes

#### Scenario: An unsupported exchange refuses the write

- **WHEN** the atomic exchange is not available on the filesystem
- **THEN** the write is refused and the file is byte identical, with no plain-rename fallback

#### Scenario: Two concurrent daemon writes do not interleave

- **WHEN** two adds of the same name are issued concurrently
- **THEN** both complete, one is refused as a duplicate rather than as stale, and the file
  contains exactly one of them

#### Scenario: Routine archives are bounded

- **WHEN** more than ten routine writes have occurred
- **THEN** the oldest routine archive has been discarded

### Requirement: Every operation has one commit point, and once it passes, the operation always completes

Add, remove and reload each span several independently fallible steps: a config replacement, a
registry mutation, a teardown, an override deletion, an OAuth record deletion, and a catalog
eviction. A partial failure must not leave the declaration and the persistent runtime state
disagreeing. Rollback is not available for a removal, because the teardown latch is terminal by
design, so the ordering below removes the need for one.

- **Nothing before the commit point mutates anything.** Every pre-commit step is a read or a
  check: name validation, declaration validation, the shutdown-latch check, the staleness
  comparison read fresh from disk, the duplicate or existence check against those same bytes, and
  writing the temporary file. The atomic exchange is the commit point and the first mutation of
  anything the user can observe.

  An earlier draft tightened the existing file's mode before the exchange, to keep a displaced
  permissive inode from staying readable if archiving then failed. That does not work: a hand
  editor can atomically replace the file with a fresh permissive inode after the check, and the
  exchange then displaces that one. The mode problem is solved after the commit instead, per the
  credential requirement, which is also what lets this rule stay absolute.

- **The shutdown latch is checked before the commit, not at registration.** A refusal after the
  commit would leave the declaration written and the registry without it, so an add that arrives
  during shutdown SHALL be refused while it can still change nothing.

- **Nothing after the commit point may abort the operation.** A pre-commit failure is an error;
  everything after the exchange is a warning that does not fail the operation. Archiving and
  archive retention happen after the exchange, so treating either as an error would leave the
  file committed and the registry unchanged, which is the exact inconsistency this requirement
  exists to prevent.
- **The displaced inode is restricted before anything fallible touches it.** Immediately after
  the exchange, and before archiving, the daemon SHALL restrict the displaced file's mode. This is
  the step that closes the credential exposure, and it must come first because archiving is the
  part that can fail.
- **A failed archive keeps its evidence.** If archiving the displaced version fails, the
  displaced file SHALL be left in place under its temporary name rather than removed. It is
  already restricted by the step above, and it sits in a directory the daemon keeps at 0700.
- **Reachability is explicitly not part of this.** A declared backend that cannot connect is a
  normal designed state, reported as down with its cause and retried. A connection attempt is
  never a precondition for committing a declaration, and the catalog refresh that follows an add
  is a trigger rather than a validation. Requiring a successful dial before commit would make
  it impossible to declare a backend whose provider is momentarily down.
- **Registry mutation follows the commit and cannot fail.** Sessions dial lazily, so adding only
  constructs an object and removing only deletes a map entry before tearing down. Neither can
  strand a committed write.
- **A removal tears down only after its declaration is gone from the file.** The reverse order
  would kill a live backend for a write that then gets refused.
- **An add cleans up before it registers.** The hygiene deletion runs after the commit but
  before registration, because a registered backend is immediately routable and a dial that beat
  the deletion would authenticate with the very record a removal was supposed to have deleted.
- **Post-commit cleanup is idempotent and does not abort on error.** Catalog eviction, override
  deletion and OAuth record deletion each tolerate an absent target, and a failure in one is
  reported while the rest still run.
- **A crash after the commit self-heals** through startup reconciliation.

#### Scenario: A refused write leaves everything untouched

- **WHEN** a config write is refused for any reason
- **THEN** the registry, the running backend, the override entry, the stored token and the
  file's content are all unchanged

#### Scenario: A failing archive still completes the operation

- **WHEN** the exchange succeeds but archiving the displaced version fails
- **THEN** the add completes, the failure is reported, and the displaced file is left readable
  by its owner only

#### Scenario: An added backend does not authenticate with a predecessor's token

- **WHEN** a backend is added under a name that still carries a stored token whose identity
  matches
- **THEN** the token is deleted before the backend is registered, so no dial can present it

#### Scenario: A cleanup failure still removes the backend

- **WHEN** post-commit cleanup fails partway
- **THEN** the backend is still removed, the remaining cleanup steps still run, and the failure
  is reported

#### Scenario: An unreachable declaration is still committed

- **WHEN** a declaration is added whose backend cannot be reached
- **THEN** it is committed and shown as down with its cause, rather than refused

### Requirement: One operation runs at a time, and every state writer proves its backend is still declared for as long as it takes to write

Serializing only the config write is not sufficient. If serialization ended at the commit point,
a removal could commit, a concurrent add of the same name could commit and register, and the
first removal's cleanup would then delete the new backend's registry entry, its tools, its
override and its stored token. So add, remove and reload SHALL each hold a single operation lock
across the whole sequence, including the registry mutation, the teardown and the cleanup.

That lock is still not sufficient, because it covers only those three operations. Two other
paths write name-keyed state and neither is one of them: a disable or enable persists an
override, and a lazy token refresh persists a token from inside the OAuth token source. Either
can write state for a backend a removal has just cleaned up.

Extending the operation lock to cover them is not the fix. A token refresh runs underneath the
backend's `life` lock, so taking the outermost operation lock there would invert the lock order
and can deadlock against a removal that holds the operation lock while waiting on that same
backend's teardown.

Instead both paths SHALL hold a read lock on the declared-set snapshot from the moment they
check that the backend is declared until their state file is fully replaced, and SHALL NOT
persist when the backend is no longer declared, or, for a token, when the declaration identity
no longer matches. A removal and a reload SHALL take the write lock and drop the name before
they begin cleanup. Checking and then releasing before writing would leave a
time-of-check-to-time-of-use gap that a removal could land inside. The guarantee needed is not
"was it declared a moment ago" but "it was still declared when this write landed, so the
cleanup that follows will see it".

The operation lock's cost is stated rather than hidden: a teardown can block on in-flight work,
and the web surface deliberately leaves a transition running after its HTTP response has timed
out, so a concurrent add can time out at the HTTP layer while a slow removal finishes. These
operations are human clicks, and the alternative is generation-tagged cleanup, which is more
machinery guarding a race that serialization removes outright.

Serializing the three operations against each other is still not enough for a reload replacement,
because enable and disable are not among them. A replacement captures the existing backend's
enabled state, removes it, and re-adds it with that state, and a disable landing between the
capture and the re-add would be reported as succeeding while the replacement comes up enabled. An
enable landing there loses the same way.

A replacement SHALL therefore hold the outgoing backend's transition lock continuously from the
capture through the teardown, and SHALL complete every hand-off of persisted state before it
publishes the replacement. Holding the outgoing lock is not sufficient on its own, because a
transition lock belongs to a backend object rather than to a name: once the replacement is
published, an enable or a disable takes the new object's lock and cannot see the old one being
held. Publication is therefore the last step, so there is no window in which a caller can reach
the replacement before its state is settled.

This is why the lock order treats the registry lock as never held while any backend lock is
acquired. That is what allows a replacement to hold the transition lock across its own map
mutation without inverting anything: no path holds the registry lock and then reaches for a
backend lock, so the reverse is safe.

Shutdown SHALL be serialized with these operations too, and SHALL latch the registry so that no
later operation can publish a backend. Every one of the three SHALL check that latch before it
mutates anything, and reload is not an exception: it registers backends exactly as an add does, so
a reload queued behind shutdown would otherwise adopt declarations, publish them, and trigger a
refresh that starts a stdio child after shutdown had already walked the registry, leaving that
child alive after the daemon exits.

The check SHALL come before an add's commit point rather than at registration, because a refusal
after the commit would write the declaration and never register it, and because registration is
required to be infallible. Shutdown walks the backend map and name list directly, and until
now that was safe because nothing ever mutated them after construction. Making them mutable turns
a concurrent shutdown into a data race, and worse, an add that lands after shutdown has walked
past its position would leave a stdio child alive after the daemon exits. The existing per-backend
shutdown latch does not cover this, because the backend that outlives the daemon is one that was
never in the map when shutdown read it.

#### Scenario: An add during shutdown leaves no surviving child

- **WHEN** an add is issued concurrently with shutdown
- **THEN** it is refused before it writes anything, no backend map access races, and no stdio
  child outlives the daemon

#### Scenario: A disable concurrent with a replacement is not lost

- **WHEN** a disable is issued while a reload is replacing that backend's declaration
- **THEN** the disable either applies to the replacement or is refused, and it is never reported
  as succeeding while the replacement comes up enabled

#### Scenario: A concurrent removal and add of one name stay consistent

- **WHEN** a removal and an add of the same name are issued concurrently
- **THEN** the declaration and the runtime agree afterwards, with the later operation's outcome
  intact

#### Scenario: A reload concurrent with an add loses neither

- **WHEN** a reload and an add are issued concurrently
- **THEN** both are applied or the later one is refused, and neither leaves the declaration and
  the runtime disagreeing

#### Scenario: An override write racing a removal never survives it

- **WHEN** a disable persists an override while the same backend is being removed
- **THEN** the write either lands before the cleanup and is deleted by it, or is refused, and it
  never survives the removal

#### Scenario: A token refresh after removal writes nothing

- **WHEN** a lazy token refresh completes after its backend has been removed
- **THEN** no record is written

### Requirement: Name-keyed state is bound to the declaration it belongs to, and is inert under any other

An override entry and a stored OAuth record are both keyed only by backend name, and the name
says nothing about which declaration they were written for. Deleting them when a declaration is
repointed or removed is therefore a hygiene measure and not a control, because it can be
defeated three ways: the change can happen by hand while the daemon is not running, so no
reload ever sees it; the deletion can fail, and post-commit cleanup is deliberately
non-aborting; and a future code path could reasonably forget to call it.

Therefore both kinds of state SHALL persist the declaration identity they were written under,
and each SHALL be compared against the current declaration before it is honoured. The identity
is every declaration field whose change invalidates the state: the resource URL, the auth mode,
and the transport kind. Comparing the resource URL alone is not sufficient, because an
unchanged URL would hide a change to either of the others. An identity that is absent or
incomplete counts as a mismatch rather than as a match.

- **A stored OAuth record whose identity does not match is unusable.** The store discards it,
  deletes it, and reports the backend as needing authorization, so a token is never presented
  under a declaration it was not issued for.

  The comparison SHALL happen where the authorization handler is obtained, not only where a
  record is read from disk. The store caches one handler per backend name and returns it without
  rereading, and that handler owns a live token source holding the token in memory, so a check
  placed on the disk read alone would never run for a primed handler. Deleting a record SHALL
  therefore also discard that backend's cached handler, its token source and any pending
  authorization, or the deletion removes only the copy that was not being used.

- **An override entry whose identity does not match is rebound to the current declaration and
  honoured, not discarded**, as long as the name is still declared. An entry for a name that is
  not declared is deleted, as ever. A disable outranks a needs-auth: when a backend's record is
  discarded while its disable is rebound, the backend reports `disabled`, and needs-auth surfaces
  only once it is enabled and dials.

  This is the opposite of the rule for a token, and the asymmetry is deliberate. A mismatch
  cannot distinguish a stale entry left by a removal whose cleanup failed from a live entry whose
  declaration was simply repointed, because telling those apart needs a per-declaration
  generation counter that this change does not have. So the tie is broken toward the safer
  failure. For a token, safe means refusing to use it. For a disable, safe means keeping it:
  enabling a backend the user disabled can start a process they stopped, whereas keeping a
  disable the user no longer wants costs one click on Enable.

  This also makes the upgrade path correct. Every override entry that exists today records a
  name and nothing else, so a rule that treated an absent identity as a discard would silently
  enable every currently disabled backend on first start after this change, stdio children
  included.

This is the control. The deletions elsewhere in this capability remain, because leaving dead
state on disk is still wrong, but nothing depends on them succeeding.

#### Scenario: A token whose identity does not match is never presented

- **WHEN** a stored record's declaration identity differs from the current declaration
- **THEN** the record is discarded and deleted, the token is never sent to the provider, and the
  backend reports needs-auth

#### Scenario: A matching token is used normally

- **WHEN** a stored record's declaration identity matches the current declaration
- **THEN** it authenticates as before

#### Scenario: A record with no stored identity is a mismatch

- **WHEN** a record written before this change carries no declaration identity
- **THEN** it is treated as a mismatch rather than as a match

#### Scenario: An offline repoint does not authenticate against the new endpoint

- **WHEN** a declaration's URL is changed by hand while the daemon is stopped
- **THEN** the stored token is not presented to the new endpoint on the next start

#### Scenario: Deleting a record also discards the live handler

- **WHEN** a backend's authorization handler has already been built and its record is then
  deleted by a removal or a repoint
- **THEN** the cached handler, its token source and any pending authorization are discarded too,
  so a re-added or repointed backend cannot authenticate from memory

#### Scenario: A mismatched override entry still disables a declared backend

- **WHEN** an override entry records a different declaration identity, or none at all, and its
  backend is still declared
- **THEN** the entry is rebound to the current declaration and honoured, so the backend stays
  disabled

#### Scenario: Existing disables survive the upgrade

- **WHEN** the daemon starts for the first time after this change, against an override file
  written before it and carrying no identities
- **THEN** every recorded disable is still in force and no disabled stdio child is started

### Requirement: A newly registered backend takes its enabled state from its caller, and no rejected operation deletes any state

State keyed by backend name would otherwise silently apply to a later backend that reused the
name: a disabled flag would leave the new backend mysteriously off, and a stored token would be
dead weight at best.

The obvious fix, deleting that state before the add commits, is wrong. Any pre-commit step that
can fail after the deletion, including the commit itself, then rejects the add having already
destroyed a live backend's token or override. Staging the deletion reversibly would fix that
but introduces the rollback path the commit-point requirement exists to avoid. So the state is
neutralized at the point of use instead, which needs no rollback and cannot fail:

- **A runtime add never consults the override store; its caller supplies the initial enabled
  state.** A user adding a backend from the surface has not disabled it, so that path supplies
  enabled and a stale disabled flag cannot affect it. A reload replacing a changed declaration
  supplies the state it captured from the backend it replaces, so a disabled backend stays
  disabled across a declaration edit. Reading the store inside the add would satisfy the second
  case and break the first.
- **The identity binding makes on-disk state inert across a restart**, which is what keeps the
  previous point true after the process exits.
- **A removal deletes the backend's override entry and its stored OAuth record**, and an add
  deletes anything it finds under its name, but both do so after their commit point, as
  hygiene, reported on failure and depended on by nothing.
- Removal also deletes a declaration the user may have hand-written, so the surface SHALL
  require a second confirming action, as the inspector already does for a destructive tool; as
  there, the confirmation guards a misclick and is not a security control.

One residual is disclosed rather than mitigated. If a removal's deletion fails, and a later
add's deletion also fails, and the declaration is byte identical, then the surviving state's
identity matches and it is honoured: a token is reused rather than re-authorized, and a
disabled flag applies. Distinguishing that case needs a per-declaration generation counter,
which means either writing daemon bookkeeping into the user's file or adding a third durable
store, and neither is worth it for a case that requires two reported failures plus an identical
redeclaration. The token reuse is not a disclosure, because the token is presented only to the
provider and resource it was issued for.

#### Scenario: Removal requires a second confirming action

- **WHEN** the user triggers a removal from the status surface
- **THEN** the surface requires a second confirming action before the request is sent

#### Scenario: A removed backend leaves no stored token

- **WHEN** an authorized OAuth backend is removed
- **THEN** its stored record is deleted, and re-adding the same name requires a fresh
  authorization

#### Scenario: An add rejected as a duplicate damages nothing

- **WHEN** an add is refused because the name is already declared
- **THEN** the existing backend's stored token and override entry are untouched

#### Scenario: An add rejected before the commit damages nothing

- **WHEN** an add is refused because the atomic exchange is unavailable
- **THEN** the stored token and override entry under that name are untouched

#### Scenario: A backend added over a stale disabled flag starts enabled

- **WHEN** a backend is added from the surface under a name that carries a disabled override
- **THEN** it starts enabled, because the runtime add never consults the store, and its hygiene
  deletion removes the entry so a restart agrees

#### Scenario: A failed hygiene deletion fails toward disabled

- **WHEN** an add's hygiene deletion of a stale disabled entry fails, and the daemon is later
  restarted
- **THEN** the backend comes up disabled rather than enabled, which is the safer of the two
  wrong answers, and the original deletion failure was reported when it happened

#### Scenario: A disabled backend stays disabled across a declaration edit

- **WHEN** a disabled backend's declaration is edited and reloaded
- **THEN** it is still disabled and its stdio child is not started

### Requirement: Declarations can be reloaded without a restart, and a reload is all or nothing

Reload is what adopts a hand edit and what clears a staleness refusal without a restart.

A reload SHALL read, validate and adopt while holding the writer mutex, and SHALL apply nothing
if the file does not load or any declaration in it is invalid. All-or-nothing validation is what
stops a malformed hand edit from taking down a working backend, and it is the level at which
that risk can be closed.

A reload applies a point-in-time snapshot, and the guarantee is stated that way rather than as
"the file's current content", because the stronger claim is not deliverable. Reading, validating
and adopting inside one mutex hold makes those three steps atomic with respect to the daemon's
own writes, which is all a lock the editor does not take can buy. A hand edit landing after the
read is simply a later state that this reload does not apply; the next reload does. What makes
that safe rather than silent is the baseline: the adopted bytes become it, so the divergence is
detected the next time the surface tries to write, and the user is told the file changed on disk
rather than having their edit displaced.

A reload SHALL adopt the exact bytes it validated as the writer's new baseline before it
completes. Without this, reload would reconcile the runtime while every later write from the
surface still refused as stale until the next restart.

A reload SHALL leave an unchanged backend untouched, because rebuilding every backend would
drop live sessions, terminate healthy stdio children, and force an OAuth backend through a
handshake it did not need.

A name that has disappeared is a removal and is treated as one, including its state deletion. A
changed declaration under an unchanged name is a replacement. A replacement SHALL capture the
existing backend's enabled state before tearing it down and SHALL supply that same state when
it registers the replacement. Otherwise editing a disabled backend's declaration would silently
enable it, and for a stdio backend that means starting the process the user disabled.

A replacement that preserves a disabled state SHOULD also re-persist the override entry under
the new declaration identity, so the recorded state matches the declaration it now belongs to.
This is hygiene, not the control: the rebind-and-honour rule already keeps such an entry in force
whether or not this runs, which is what closes the window where the daemon crashes partway
through a replacement.

A replacement SHALL delete the stored OAuth record when the declaration identity changed. This
is hygiene layered on top of the identity binding, which is what actually prevents the token
from being used under the new declaration.

A replacement is applied even when the new declaration turns out to be unreachable: the file is
what declares intent, and a declared backend that cannot connect shows as down. A reload can
therefore leave a previously working backend red, when that is what the edited file says.

#### Scenario: A malformed file changes nothing

- **WHEN** a reload reads a file that does not parse, or that contains one invalid declaration
- **THEN** nothing is applied and every existing backend keeps its session

#### Scenario: An edit arriving after a reload read is detected, not silently displaced

- **WHEN** the file is edited by hand after a reload has read it
- **THEN** that reload applies the snapshot it read, and the next write from the surface is
  refused as stale rather than overwriting the edit

#### Scenario: A write immediately after a reload is not refused

- **WHEN** the user adds a backend from the surface right after a reload
- **THEN** the write is not refused as stale, because the reload adopted the bytes it validated

#### Scenario: A hand-added backend appears on reload

- **WHEN** the user adds a declaration by editing the file and then triggers a reload
- **THEN** the new backend is registered and its tools are listed

#### Scenario: A hand-removed backend is torn down on reload

- **WHEN** a declaration is deleted from the file and a reload is triggered
- **THEN** that backend's session closes, its tools leave the catalog, and its stored token is
  deleted

#### Scenario: A repointed declaration returns to needs-auth

- **WHEN** a reload applies a declaration whose URL changed
- **THEN** the backend reports needs-auth rather than authenticating with the old token

#### Scenario: A disabled backend stays disabled across a repoint and a restart

- **WHEN** a disabled backend's URL is edited, a reload applies the replacement, and the daemon
  is then restarted
- **THEN** the backend is still disabled, because the override entry was re-persisted under the
  new declaration identity rather than left carrying the old one

#### Scenario: An unrelated change keeps the stored token

- **WHEN** a reload applies a declaration whose change does not affect its identity
- **THEN** the stored token is kept and no re-authorization is needed

#### Scenario: An unchanged backend survives a reload

- **WHEN** a reload adds, removes or replaces one backend
- **THEN** every other backend keeps its session, its stdio child, and its authorization

### Requirement: Runtime state is reconciled against the declarations at start

At startup the daemon SHALL delete any override entry and any stored OAuth record whose backend
is not declared. This is the backstop for a crash between the commit point and the cleanup, and
it is the only thing that catches a declaration removed or repointed by hand while the daemon was
not running.

For state whose backend IS declared but whose identity does not match, startup SHALL follow the
same split as everywhere else, and SHALL NOT delete both kinds alike: a stored OAuth record is
discarded and deleted, and an override entry is rebound to the current declaration and honoured.
Deleting a mismatched override here would undo that rule at the one moment it matters most, the
first start after an upgrade, when no entry carries an identity at all.

The cost is deliberate and worth stating: a user who deletes a declaration by hand and later
restores it gets a fresh authorization rather than the old token. That is the same outcome as a
removal from the UI, and one rule for both is easier to reason about than a token that survives
some removals.

#### Scenario: Orphaned state is gone after a start

- **WHEN** the daemon starts with an override entry and a stored OAuth record for a backend
  that is not declared
- **THEN** both are deleted, and a declared backend's own matching state is untouched

#### Scenario: Startup resolves a mismatch per store, not uniformly

- **WHEN** the daemon starts with a stored OAuth record and an override entry that both record
  an identity differing from their still-declared backend
- **THEN** the record is deleted and the override entry is rebound, and the backend reports
  `disabled` rather than needs-auth, because a health record carries one state and the disable
  outranks it; needs-auth is reported later, once the backend is enabled and dials

### Requirement: Writing the file does not widen access to a credential

A declaration can carry an inline credential in `env` or `headers`, and an archived version
holds the same credentials as the file it came from. The daemon SHALL leave neither the file,
nor the inode it displaces, nor any archive readable beyond its owner.

**The containing directory is the control, and a per-file mode is defence in depth.** Stating the
precedence this way is what keeps the guarantee consistent with the commit-point rule, which
requires every post-exchange step to be a warning rather than an abort. A `chmod` can fail, so a
guarantee resting on it would force a choice between aborting after the commit and knowingly
retaining a readable credential; a guarantee resting on the directory does not, because the
directory is established once at startup rather than per write.

The daemon SHALL therefore ensure the resolved declaration directory is 0700 at startup, and
SHALL refuse to write declarations at all if it cannot. That check happens before any write
exists to be committed, so refusing is free.

Given that, the remaining points are ordered but not load-bearing on their own. The displaced
inode SHALL be restricted immediately after the exchange, before archiving or any other step that
can fail, and a failure to do so is a warning: the file stays inside a 0700 directory and is not
deleted, so neither confidentiality nor recoverability is given up. Tightening the file before the
exchange instead does not work at all, because a hand editor can atomically replace it with a
fresh permissive inode after the check and the exchange then displaces that one; the mode of the
inode that was checked is not the mode of the inode that gets displaced.

The new inode is created restricted, since the temporary file is created at 0600. Each archive is
written at 0600, and an existing archive found more permissive is tightened. No path loosens
anything.

#### Scenario: A permissive mode is tightened on write

- **WHEN** the daemon writes a configuration file that is readable beyond its owner
- **THEN** the written file and its archive are readable by their owner only

#### Scenario: A displaced file is restricted even when archiving fails

- **WHEN** archiving fails and the displaced file is left under its temporary name
- **THEN** that file is readable by its owner only

#### Scenario: A file replaced by an editor mid-write is still not left exposed

- **WHEN** the file is atomically replaced by a permissive inode after the daemon read it, and
  the daemon's write then displaces that inode
- **THEN** the displaced inode is restricted before archiving is attempted, and the directory
  holding it is at 0700
