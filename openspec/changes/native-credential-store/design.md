# Native credential-store support for mcpd

## Context

See `proposal.md` for the observed credential-delivery problem. The current implementation resolves backend environment values and HTTP headers from the daemon environment, and the embeddings client reads its API key from the same source. A supervised daemon or an agent sandbox can lack variables that are available in an interactive shell.

The design must preserve the single-binary, `CGO_ENABLED=0` release, least-privilege child environments, loopback-only control plane, and at-most-once backend dispatch. It must also keep legacy configurations environment-only unless an operator opts in.

## Goals / Non-Goals

**Goals:**

- Resolve missing `${NAME}` references from a configured secret store.
- Support native credential stores on macOS and Linux through maintained integrations.
- Preserve process environment variables as the highest-priority source for backward compatibility.
- Fail closed with bounded, actionable diagnostics when the selected native store is absent, locked, inaccessible, or blocked on interaction.
- Provide an explicit file-store mode for headless systems that cannot use a native store.
- Add CLI and local web-panel operations to set, inspect status for, and remove configured secret names without exposing stored values.
- Refresh affected backend connections after a secret changes.

**Non-Goals:**

- Automatically unlocking a native credential store.
- Automatically falling back from a native store to a file store.
- Enumerating every credential stored under the native provider's service namespace.
- Cloud secret managers, hardware security modules, and shared multi-host secret synchronization.
- Changing mcpd's existing local control-plane trust boundary or making the dashboard remotely accessible.
- Migrating arbitrary literal values already present in existing configuration.
- Fixing the separate Grafana LiteLLM upstream Host-header rejection.

## Decisions

### Configuration and resolution behavior

Add a small secret-store configuration block with a selected provider. Native storage is the recommended provider. File storage must be selected explicitly and must use a user-owned file with restrictive permissions in mcpd's state directory. Omitting the block preserves legacy environment-only behavior and does not probe a native store during upgrade.

When mcpd expands `${NAME}`:

1. If the variable is present in the process environment, use its value, including an intentionally empty value, and do not query the provider.
2. If the variable is absent and a secret provider is configured, query that provider by the exact variable name.
3. If the provider cleanly reports not found, preserve current behavior: log the existing missing-variable warning and expand the reference to `""`. The same clean miss for an embedding key returns `""`, as it does today.
4. If the configured provider is unavailable, locked, denied, timed out, wedged, corrupt, or otherwise fails, return a typed resolution error for the dependent consumer. The error identifies the name and provider condition but never includes a secret value.

With no secret-store block, expansion follows the current implementation exactly: a present variable, including an empty one, supplies its value; an absent variable logs a warning and becomes `""`. Secret-store support does not turn a clean absence into a hard failure.

Do not silently copy environment values into persistent storage. Do not silently change providers. The provider-backed resolver has a normative allowlist: backend environment values, backend HTTP headers, and embedding credentials that currently depend on environment variables. Every other current or future `${NAME}` expansion remains environment-only unless a later design explicitly assigns it a consumer and lifecycle. The secret-store configuration block itself is always parsed with literal or environment-only semantics and never queries the provider, so provider construction cannot depend on the provider it is constructing.

### Secret-store interface and providers

Introduce a narrow internal interface with get, set, and delete operations. Each operation accepts a context. Inject the interface where configuration references are resolved so tests can use an in-memory fake.

Stored secrets are non-empty printable UTF-8 text, up to a documented portable 2048-byte limit. Reject invalid UTF-8, NUL, C0 control characters, and DEL at set time. Spaces, printable Unicode, quotes, backslashes, dollar signs, and backticks are preserved byte-for-byte. An intentionally empty process environment variable remains supported, but persistent secrets are non-empty by definition. The provider contract requires `Get(Set(v)) == v` byte-for-byte for every accepted value.

The state directory is common security infrastructure for both providers because native mode stores its lock and helper marker there. On first daemon or offline-CLI use, create it with mode 0700. Before creating or trusting any store, lock, or marker artifact, verify that the directory belongs to the current daemon identity, has no group or other access, and has no unsafe writable parent. If validation fails, disable the selected provider with a typed permission-validation condition. Native-only mode receives exactly the same directory creation and validation as file mode.

Use `github.com/zalando/go-keyring` v0.2.8 or newer as the initial native-store adapter only if its platform behavior satisfies all adoption requirements below. Its verified v0.2.8 public API provides Set, Get, Delete, and DeleteAll, but no enumeration operation. Therefore mcpd does not maintain a native-store index and does not promise native credential enumeration.

Secret status is computed from exact `${NAME}` references found in the loaded mcpd configuration. The effective source is first-class: `environment; provider not checked`, `provider-present`, `absent`, or a typed provider condition such as unavailable, locked or denied, busy, timed out, wedged, or unexpected failure. Environment-satisfied names never cause a provider probe during startup or polling, even when the file provider could be enumerated. Therefore ordinary status does not claim whether a stored value also exists. A successful set response can state that the value it just stored is shadowed, but after restart the uniform status returns to `environment; provider not checked`. This includes referenced-but-missing names and excludes unreferenced credentials without causing unnecessary keychain access.

The native provider uses an mcpd-specific service namespace. The secret name is the credential account/key. Provider errors are normalized without discarding the underlying error category needed for diagnostics. The implementation maintains a documented per-platform mapping from native/library results to mcpd's typed conditions.

A native adapter has seven blocking adoption requirements on each platform: bounded execution, reliable process-tree termination, faithful error categorization, no secret material in the argv or environment of the library or any descendant process, atomic replacement of an existing credential, non-interactive read-back by mcpd running under the same intended user/session identity, and byte-exact round-trip fidelity for every accepted value. Verify these properties against the selected version's source and with executable process-tree inspection, fault-injection tests, and real-provider set-then-get integration tests before enabling that platform. Provider-specific size errors map to a typed `value too large` condition naming the actual platform limit. Any value class a platform cannot round-trip must be rejected before storage with a typed validation error.

For `go-keyring` v0.2.8 on macOS, the source check confirms Set starts `/usr/bin/security -i`, sends the encoded command through stdin, and issues one `add-generic-password -U` operation rather than an explicit delete-then-add sequence. The implementation still verifies the full descendant tree and kills the helper at controlled points during replacement to prove the prior credential remains readable. Linux adoption requires a single Secret Service `CreateItem` with replacement enabled. If a platform adapter violates any adoption invariant, use a native API adapter or stdin-safe atomic invocation there instead of enabling that implementation.

The explicit file provider stores values only in mcpd's validated state directory. It uses a dedicated `secrets.lock` sidecar that is created with restrictive permissions and is never renamed or replaced. Every daemon and offline CLI read-modify-write operation takes an exclusive `LOCK_EX|LOCK_NB` lock on that sidecar and holds it through reading, temporary-file creation, writing, syncing, replacement, and directory syncing. Lock acquisition is non-blocking with bounded retry until the caller's context deadline. Deadline expiry returns a typed `lock contended` provider error.

Pure file-provider reads do not take the sidecar lock. They open and parse one immutable old-or-new snapshot supplied by atomic replacement. A missing data file is an empty first-run store. A present but malformed snapshot is a hard provider failure.

For replacement, create a temporary file in the target directory with `O_CREAT|O_EXCL|O_WRONLY` and explicit mode 0600 before any secret bytes are written. Write the complete encoded store, fsync the temporary file, close it, rename it over the target, then fsync the parent directory. Never rely on umask and never chmod a populated temporary file after writing. A corrupt, truncated, or undecodable existing store is a hard provider failure and is never treated as an empty store.

Startup rejects a store not owned by the daemon user, a store with any group or other permission, or a parent directory writable by group or other. The documentation states that file mode protects against other users but not processes already running as the same account.

### Bounded provider calls

Every provider caller, including offline CLI file operations, supplies a short configurable deadline with a single-digit-second default. No provider operation can block daemon startup, backend supervision, a CLI request, or a dashboard request beyond that deadline.

Run native credential operations in a hidden, short-lived mcpd helper subprocess rather than in the daemon process. The helper uses the native library, accepts set values only through an inherited pipe, and returns structured results through a pipe. Secret values never appear in arguments, environment variables, diagnostics, or logs. Native get, set, and delete use the same logical service/name namespace.

Permit only one native helper operation across the daemon and offline CLI processes. Each process first acquires a context-bounded in-process slot, then a dedicated never-replaced `native-helper.lock` sidecar with the same non-blocking, deadline-bounded OS locking pattern as the file store. If acquisition expires, read the atomically maintained helper marker without the lock. Report `provider wedged` only when the marker is explicitly `phase=wedged` and identifies a matching live helper. A healthy `phase=in-flight` marker is ordinary `provider busy`. Busy is local contention: do not cache it as a negative provider result and do not latch it as provider health. Pending/background work uses a separate contention schedule that starts at 250 milliseconds, doubles to a two-second cap, and resets after slot acquisition, so busy retries are paced without being treated as provider failures.

Every successful lock acquirer, not only startup, validates the marker before spawning. A `phase=wedged` marker with a matching live helper enters the explicit bounded cleanup path. A valid `phase=in-flight` marker with a live owner/helper is treated as busy and never latched as provider health. An in-flight marker whose owner is gone or whose termination deadline has elapsed enters bounded recovery. A stale or invalid marker is removed durably. No acquisition path can proceed directly from lock acquisition to spawn without this check.

The helper protocol includes an explicit platform-specific health operation before every get, set, and delete. On Linux, confirm a session D-Bus connection, a Secret Service name owner, the configured collection object, and its unlocked state.

On macOS, preserve the existing `CGO_ENABLED=0` release pipeline. The helper uses bounded `/usr/bin/security` subprocesses inside its isolated process group. It resolves and confirms the user default keychain with `security default-keychain -d user`, then performs the requested item operation in the same helper invocation. For get, only the documented item-not-found result from `find-generic-password -w` is a clean miss; any other error is typed, and a prompt or deadline is interaction-required. Set and delete run the same keychain-existence preflight before their bounded operation.

There is no reliable non-prompting macOS lock/authorization probe in this no-cgo design. A disposable locked-keychain test showed that `security show-keychain-info` itself blocks on interaction and returns user-canceled, so it is not used as a background health probe. After a macOS operation reaches `interaction-required`, automatic pending-queue, reconnect, and status work must not run another value-bearing `security` operation. The provider remains interaction-required until the user selects explicit Retry in the CLI or panel, which permits one bounded attempt. This is an intentional platform limitation that prevents recurring credential prompts. Linux retains automatic recovery because its health checks are non-interactive. If this subprocess mechanism fails the seven adoption gates, macOS native support remains disabled rather than adding an unplanned cgo dependency.

At startup, build work as consumer groups, not independent names. A consumer group is one backend or the embedding client together with all unresolved secret references it requires. Groups whose references are all satisfied by the environment need no helper. Resolve every required name for one group serially within the aggregate deadline, construct that consumer only after the full set succeeds or cleanly misses, then release the temporary resolved-value set. Cache presence status separately. There is no separate presence-probe pass.

If any name in a group cannot resolve because the aggregate budget expires or provider health blocks it, discard every temporary value already fetched for that group and enqueue the whole group. The background worker later re-resolves the group's full set before construction. A locked, unavailable, denied, interaction-required, or timed-out result becomes provider-level health and short-circuits later groups without starting more helpers. Fully resolved groups cause one get per provider-backed name before construction. A pending group may re-fetch names from its failed partial attempt, but no secret value is held across the pending interval.

Use one pending-resolution queue for every consumer group not constructed during startup. This includes groups skipped because the aggregate deadline expired and groups short-circuited by locked, unavailable, timed-out, interaction-required, or wedged provider health. Their backends report `pending secret resolution`, with the provider condition attached when known. Daemon startup completes without synchronously constructing them.

A bounded background worker resolves pending groups serially through the same global slot after the applicable provider-health or contention backoff. Provider-busy leaves the group pending and uses contention pacing. Non-interactively detectable provider-health recovery, including wedge revalidation, requeues and drains the same queue. On macOS, interaction-required suspends automatic native work until explicit operator retry. Pending backends otherwise transition to connected, missing-secret, or provider-error without operator action. Explicit retry can enqueue them, but does not bypass the global slot.

After acquiring both operation locks, create pipes and launch the helper blocked waiting for its request. Give it a non-secret, random 128-bit instance identifier in its hidden-helper argument and environment so descendants inherit a verifiable non-secret tag.

On POSIX, create a private session in the child pre-exec path and confirm from the parent that `getsid(helperPID) == helperPID` and `getpgid(helperPID) == helperPID`; abort before sending a request if the helper is not both session leader and process-group leader. Record a restrictive, atomic `native-helper.json` marker only after this confirmation. The marker contains no secret. It contains `phase=in-flight`, the operation deadline, the later termination-attempt deadline, the owner PID/start time, canonical helper executable path, hidden-helper identifier, helper PID, confirmed session and process-group identities, process start time, and daemon/CLI instance identifier. Then send the request through the pipe.

The helper exits immediately if the request pipe reaches EOF before a complete request. On process-group setup, marker-write, or request-write failure, the parent closes the request pipe, kills its own identity-proven child if necessary, and waits for it before releasing locks. This closes the pre-marker orphan window while ensuring no native operation begins before recoverable identity is durable.

Launch the helper in an isolated POSIX session and process group. On operation deadline, request termination of the whole tree, escalate to `SIGKILL`, and wait only for a separate bounded termination interval. On normal exit or confirmed termination, clear and durably sync the marker before releasing the cross-process lock and in-process slot.

If termination confirmation fails, atomically rewrite and durably sync the marker as `phase=wedged`, retain all helper identity fields, transition the native provider to recoverable `provider wedged` health, then release `native-helper.lock` and the in-process slot. The phased marker, not indefinite lock ownership, becomes the cross-process guard. Normal operations report an actionable wedged diagnostic while its backoff window is active. An explicit retry acquires the lock and performs another bounded kill-and-exit-confirmation attempt for the recorded helper. It clears the marker and wedged state only if cleanup succeeds. Never start a second helper while a phased marker identifies a matching live process.

Before sending any signal, prove that the target is an mcpd native helper. Require PID and start-time match, canonical executable-path match, the expected hidden-helper argument, and the marker's random instance identifier in the target command line. Before a group-directed signal, also require the recorded SID and PGID equal the helper PID, the verified leader still owns that session and group, both identifiers differ from the signaling process's own session and group, and every enumerated member remains in the recorded private session and group under the daemon user. Require the matching non-secret instance tag where the platform exposes member environments. If the leader is gone or any group member cannot be positively tied to the private session, do not signal the group. Signal only individually identity-proven members, otherwise retain wedged state until they exit naturally. If any leader identity check fails, treat the marker as stale, remove it durably, and send no signal.

Current macOS XNU omits environment variables for restricted target executables unless SIP permits unrestricted tracing or the caller has an Apple-private entitlement. Therefore, the macOS adapter launches each direct `/usr/bin/security` child in a separate process group inside the helper's private session. Before allowing the command to proceed, it durably records the child PID, start identity, canonical executable, parent helper identity, SID, and PGID. Termination first signals the exact direct child, then uses its private-session process group for bounded cleanup. This does not depend on reading a restricted process environment. The complete `security` descendant tree remains an adoption gate.

On daemon or offline-CLI startup, acquire `native-helper.lock` before native work and inspect any marker with those full identity checks and phase rules. If a proven helper process or process group requires recovery, perform one bounded termination and exit-confirmation attempt. If it does not exit, write `phase=wedged` and release the lock while retaining the marker. If the target is absent or fails helper identity proof, remove the stale marker durably. A restarted process never relies only on in-memory state and never starts new native work before this recovery check completes.

Wedged health self-recovers on the existing capped negative-backoff schedule. A bounded background revalidator acquires both locks, runs full marker and helper identity checks, and performs at most one bounded termination attempt per window. If the helper has exited or fails identity proof, remove the marker durably, clear wedged health, invalidate presence status, and requeue pending consumer groups. If it remains identity-proven and live, retain wedged health and back off again. The provider self-clears within one capped backoff window after the helper exits, without operator action. Explicit retry requests an immediate bounded revalidation but cannot bypass the one-helper rule.

A timed-out get reports timed out or interaction required after successful termination. A timed-out set or delete reports an indeterminate mutation outcome, never success, and explicitly warns that the prior value might have been removed if the platform adapter violated or regressed its atomicity guarantee. Invalidate the presence cache so the next explicit status performs a fresh probe. Because another operation starts only after the prior helper is confirmed exited, no late helper can overwrite a later mutation and the number of outstanding abandoned native operations remains zero.

The helper attempts non-interactive access where the platform supports it. If a platform cannot reliably terminate and reap the helper tree, native provider support fails closed on that platform until the property is implemented and tested.

Maintain a per-name presence-status cache containing only present, absent, or a typed condition, never the secret value. A successful present or absent result has a configurable minimum re-probe interval with a five-minute default. Status APIs and dashboard polling serve this cache and never initiate repeated gets inside that interval. An explicit operator refresh may probe through the normal global slot and health rules. Set, delete, external file-store change, provider-health transition, and successful operator remediation invalidate the applicable entries.

Completed provider failures and timeouts use exponential negative backoff with a cap. Reconnect loops and status polling do not probe the provider more than once per backoff window. Provider-busy uses its separate contention pacing. Backend construction and reconnect still perform a bounded get when they need the value; a cached presence result is never substituted for the value. Successful values are retained only long enough to construct the affected backend or client environment.

### Headless behavior

Native stores are user-session facilities and are not assumed to exist merely because the operating system supports them:

- macOS: a user LaunchAgent can use the login keychain after login and unlock. A boot-time LaunchDaemon can encounter a locked keychain or an authorization prompt that no user can answer.
- Linux: Secret Service normally requires a running provider, a session D-Bus, and an unlocked collection. SSH-only sessions, `systemd --user` lingering before login, minimal servers, and system services can lack one or more of these.
- Containers: host credential-store IPC and user-session identity are normally absent.

mcpd does not attempt to unlock a credential store. When native access fails, status output and logs name the operational condition and recommend either starting mcpd in the intended unlocked user session or explicitly configuring the file provider. A provider-level locked, unavailable, or wedged condition disables every backend with an unresolved `${NAME}` reference because mcpd cannot distinguish a clean absence while the provider is unhealthy; documentation states this blast radius explicitly. Backends whose references are satisfied by the environment and unrelated backends remain operational.

### CLI and web panel

Add CLI operations to set, show configuration-derived status for, and remove a secret. `set` reads from a hidden terminal prompt or standard input, never a command-line argument. A terminal prompt reads one line and removes its line terminator. Stdin reads to EOF and removes at most one final LF or one final CRLF, matching common `printf` and shell-pipe use. It performs no other trimming or normalization, then applies the printable-UTF-8 validation. Status returns referenced names and provider/status metadata only. No command returns stored values.

The CLI first attempts the running daemon's local API so a successful mutation and targeted reconnect form one operation. Before a daemon-side set, inspect the effective source. If the daemon environment defines the name, complete the store write but return a prominent success warning: the environment value still shadows the newly stored value, and the stored value will take effect only after the variable is removed from the launch environment and the daemon is restarted. A reconnect alone cannot change process environment. The panel shows the same warning while ordinary status remains `environment; provider not checked`.

If the daemon is unavailable, the CLI writes the selected provider directly so first-time and recovery setup work while mcpd is stopped. If the configured state directory is absent, the CLI creates and validates it with mode 0700. It reports that the invoking identity is now the expected daemon owner. The CLI distinguishes this fresh-install action from an ownership mismatch.

If the state directory already exists, use its owner as the expected daemon identity. Before either a native or file-provider offline write, the CLI's effective UID must match that owner. On mismatch, refuse before writing and identify both identities. For the file provider, diagnostics can include the applicable `sudo -u <owner> mcpd secret set ...` form. For the native provider, diagnostics require running inside the owner's actual unlocked login session and do not suggest `sudo -u`, because sudo normally lacks that user's Keychain or session D-Bus context. This prevents a credential from being written into an interactive user's namespace when the daemon runs as another account.

After an offline native write, success names the current credential namespace/identity. In the daemon-down path the CLI cannot know the future daemon environment, so it also states that effective-source and shadowing status cannot be determined until daemon startup.

For every offline provider write, validate the state directory with the same platform-specific permission checks used by the daemon. After every direct file-provider write, repeat full ownership and permission validation before reporting success. File-provider direct writes take the same sidecar lock as daemon writes.

After a direct write, the CLI retries a best-effort local notification. If the daemon started during the race, it receives the change and reconnects affected consumers. If it remains stopped, its next startup reads the new value. Native-store per-key writes rely on the native provider's transactional operation.

For every provider, complete the mutation, invalidate the applicable presence cache, release all in-process and cross-process locks, and only then notify or trigger targeted reconnect. A reconnect can therefore perform its own bounded get without contending with the mutation that caused it.

Add equivalent controls to the existing loopback web panel. Secret inputs are write-only. API responses, logs, HTML, and diagnostics never return the value. Reuse the panel's existing same-origin and local-access protections. A successful daemon-side change reconnects only backends that reference that secret, plus any affected embedding client.

### Error handling and lifecycle

Secret-provider initialization failure does not crash the entire daemon. Record provider health separately and mark only dependent backends unavailable.

Backend reconnects re-resolve the required names, subject to the global native-operation slot, provider-health short circuit, and negative-backoff rules. This lets an unlocked keychain recover through an explicit retry without a daemon restart while preventing automatic prompt and retry storms. File-provider changes made outside the daemon are detected by watching the state directory for create and rename events affecting the data-file name. A periodic metadata check is the fallback for platforms or filesystems with unreliable directory events. Never attach the durable watch to the replaceable data-file inode.

The daemon keeps only an in-memory digest of the last successfully loaded file contents. A daemon-side write updates that digest before watch processing resumes. Directory events are debounced, and a reload whose digest matches the last observed contents does not trigger a second reconnect. The digest is never persisted or logged. This suppresses the daemon's own write event while still detecting repeated external atomic replacements. Native changes made by the mcpd CLI notify the daemon through the local API. Out-of-band changes made with operating-system keychain tools become visible on the next bounded reconnect or explicit operator retry.

Deleting a secret reports which configured consumers depend on it. After confirmed deletion, reconnect those consumers so their status reflects the missing value.

### Files and implementation areas

- Add an internal secret-store package containing the interface, native helper client, explicit file provider, typed errors, call bounds, locking, and tests.
- Add a hidden helper entry point that performs one native get, set, or delete operation and communicates only through pipes.
- Extend configuration parsing and validation with the provider selection and file-store location rules.
- Route existing variable expansion through a resolver that checks process environment first and the configured provider second.
- Extend CLI commands and the local dashboard API/UI with write-only secret management and configuration-derived status.
- Extend backend supervision so affected consumers reconnect after a change.
- Update user documentation with input normalization, accepted value syntax, platform behavior, headless limitations, migration examples, and recovery guidance. Corrupt file-store guidance explicitly explains how to preserve the corrupt file for diagnosis, move it aside, and reinitialize because normal set/remove operations intentionally refuse a corrupt store. Permission remediation covers existing state directories, including ownership correction and mode 0700.

Exact file names follow the current repository structure discovered during implementation. Avoid unrelated refactors.

## Migration Plan

1. Ship the feature with no `secrets` block in existing configuration. Upgrade behavior is unchanged and does not probe a provider.
2. Let the operator select `native` or `file`, set allowlisted names through the CLI or panel, and verify consumer status before removing variables from the daemon launch environment.
3. Restart the daemon after removing a shadowing environment variable. A targeted reconnect alone cannot change the process environment.
4. Roll back by restoring the environment variables and removing the optional `secrets` block. Stored provider entries remain intact until the operator removes them explicitly.

## Verification Strategy

- Unit-test resolution precedence, legacy mode with no provider, present-empty environment behavior, rejected empty, control-containing, invalid-UTF-8, and oversized stored values, absent-everywhere warning and empty expansion, clean provider misses only after a successful health check, typed provider failures, backend-grouped startup resolution, aggregate-bounded startup, provider-health short circuit, presence-status caching, negative backoff, redaction, and dependency identification with an in-memory fake. Assert a reference in any non-allowlisted field and in the secret-store block causes zero provider calls. With N referenced groups and a wedged provider, assert exactly one native helper starts. Assert each fully resolved startup group causes exactly one get per provider-backed name before construction. For a group with two names where the second becomes pending, assert the first temporary value is released, the whole group is queued, and the background pass re-resolves the complete set. Assert N status polls inside the positive/absent cache interval cause at most one helper invocation per name.
- Unit-test helper request and response framing, stdin-only set values, exact terminal/stdin line-ending behavior, the portable 2048-byte bound and platform-specific too-large mapping, per-platform health mappings, process-tree timeout and bounded termination, wedged-provider poisoning and cleanup, context-bounded in-process and cross-process slot acquisition, paced provider-busy behavior, the one-operation global bound, indeterminate mutation results, phased marker durability, full helper-identity and process-group validation, pre-marker failure cleanup, and recovery through explicit retry, background revalidation, or process restart. Assert provider-busy is neither cached nor latched as provider health and never creates a hot retry loop. With two processes overlapping on a healthy long-running operation, assert the loser reports busy, never wedged. With one process in `phase=wedged`, assert a second process reports wedged rather than busy. Enter wedged at startup with N referenced names, let the helper exit, and assert automatic revalidation clears health and all pending backends connect within one capped backoff window. Inspect the full native-helper descendant process tree during set and assert no secret appears in argv or environment.
- Plant a marker naming a live same-user non-helper process and assert mcpd removes the marker without delivering any signal. Plant a marker whose PGID equals the test process's own group and assert no group signal is delivered. Simulate failed process-group creation and assert the helper receives no request. Make the common state directory group/other-accessible and assert native mode fails with the typed permission condition before reading a lock or marker.
- For every enabled native platform adapter, kill the helper at controlled points during replacement of an existing credential and assert the prior credential remains readable. Run a real-provider round-trip corpus containing quotes, backslashes, dollar signs, backticks, leading and trailing spaces, and multibyte UTF-8; assert byte equality. Assert embedded controls and non-UTF-8 are rejected before storage. Treat any failure as a platform-adoption blocker.
- Unit-test status derivation from configuration, including `environment; provider not checked`, provider source, the set-time shadow warning, referenced-but-missing names, and unreferenced stored names. Assert startup and polling do not query a provider for an environment-satisfied name.
- Unit-test the dedicated lock sidecar, bounded non-blocking lock acquisition, lock-contention typed errors, lock-free snapshot reads, missing-file bootstrap, same-directory replacement, corruption as hard failure, fsync error handling, and permission validation. Hold the lock past a caller deadline and assert bounded return. Run two concurrent writers that each set a distinct name and assert both values survive. Inject a pre-write hook and assert the temporary file is never group or other readable.
- Test CLI hidden-input and stdin paths and assert values never appear in arguments or output.
- Test daemon-available and daemon-down CLI mutation paths, fresh-install secure state-directory creation for both providers, environment-shadowed successful writes and warnings, current-identity native namespace reporting, notification races, targeted reconnection, lock release before reconnect for both providers, and cross-UID rejection for both providers. A daemon-side native set followed by reconnect must produce exactly two serialized helper invocations and no provider-busy result.
- Test dashboard handlers for set/status/remove, write-only responses, origin protection, redaction, bounded calls, and targeted reconnection.
- Test partial availability: an inaccessible or blocked native store disables only dependent consumers. Test zero provider-dependent references and healthy-provider aggregate-deadline exhaustion. Assert daemon startup completes within the aggregate budget, affected backends report `pending secret resolution`, and the bounded background worker later transitions them to connected without operator action.
- Add platform-gated native-provider integration tests that skip with a clear reason when no interactive credential service exists. Verify helper process-tree termination and non-interactive set-then-get under the intended same-user session. On macOS, lock a disposable test keychain with N pending groups and assert automatic backoff produces zero further value-bearing `find-generic-password` calls; after unlock, one explicit retry drains the queue. Do not make CI depend on a desktop keychain.
- Perform successive external atomic replacements while the daemon watches the state directory and assert the final content is applied and at least one refresh occurs after the last replacement. Space a separate test beyond the debounce window when verifying two distinct refreshes. Assert a daemon-side write produces only one targeted reconnect after its matching watch event is suppressed by the in-memory digest.
- Run Go formatting, focused package tests, the repository's full test suite, static analysis, and a clean build.
- Manually verify on the current macOS user LaunchAgent: set a Context7 key, start or reconnect the backend without exporting the key into launchd, confirm tools are advertised, and confirm logs/config/status do not contain the secret.

## Risks / Trade-offs

- Native-store availability differs by runtime context. Fail closed, expose precise health, bound calls, suppress prompt storms, and require explicit file-store selection.
- Native APIs may not support in-process cancellation. Isolate each call in a killable helper tree, serialize calls globally, reap on timeout, and short-circuit provider-level failures before probing more names.
- Dashboard and CLI handling create accidental-disclosure paths. Keep values write-only and add redaction-focused tests.
- File replacement and concurrent mutation can disclose or lose credentials. Use restrictive creation, durable replacement, advisory locking, corruption hard failures, and injected-failure tests.
- Permission semantics can differ between supported filesystems. Fail closed when POSIX ownership or mode validation is unavailable.
- Dependency refresh can disrupt unrelated MCPs. Compute consumers by exact reference and reconnect only those consumers.
