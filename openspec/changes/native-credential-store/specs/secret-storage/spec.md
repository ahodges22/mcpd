## Purpose

Defines secure and bounded storage behavior on macOS and Linux for native user credential stores and the explicit managed-file alternative.

## ADDED Requirements

### Requirement: Stored values use a portable contract
Persisted secrets SHALL be non-empty printable UTF-8 text of at most 2048 bytes. mcpd SHALL reject invalid UTF-8, NUL, C0 controls, DEL, and platform-incompatible values before storage. Every accepted value SHALL round-trip byte-for-byte.

#### Scenario: Printable value round-trips exactly
- **WHEN** a value contains printable Unicode, quotes, backslashes, dollar signs, backticks, or leading and trailing spaces
- **THEN** `Get(Set(value))` returns the identical byte sequence

#### Scenario: Invalid persistent value is rejected
- **WHEN** a set request contains an empty value, invalid UTF-8, a prohibited control, or more than 2048 bytes
- **THEN** mcpd returns a typed validation error before invoking the provider

### Requirement: Provider choice is explicit
Native storage SHALL be the recommended provider. The managed-file provider SHALL operate only when selected explicitly. mcpd SHALL not unlock a native store, copy environment values into storage, or fall back between providers automatically.

#### Scenario: Native provider is unavailable
- **WHEN** native storage is configured but no usable user-session credential service exists
- **THEN** mcpd reports the provider condition and does not create or use a managed-file store

### Requirement: State infrastructure is identity-restricted
Both providers SHALL use a common state directory that is owned by the daemon identity and inaccessible to other ordinary users. mcpd SHALL validate ownership, permissions, and unsafe writable parents before it trusts store, lock, or marker artifacts.

#### Scenario: Unsafe POSIX state directory
- **WHEN** the state directory or a relevant parent has group or other write access, or the directory is owned by another user
- **THEN** mcpd disables provider access with a typed permission-validation condition before reading a provider lock or marker

### Requirement: Native operations are isolated and bounded
Each native get, set, delete, retry, and health operation SHALL run in a short-lived helper process with a caller deadline. Set values SHALL travel only through inherited pipes. Secret material SHALL not appear in arguments, environment variables, logs, diagnostics, or status data.

#### Scenario: Native operation times out
- **WHEN** the native helper or one of its descendants does not complete before the deadline
- **THEN** mcpd performs bounded process-tree termination and returns a typed timeout or interaction condition

#### Scenario: Mutation outcome is uncertain
- **WHEN** a set or delete times out after the native operation might have started
- **THEN** mcpd reports an indeterminate mutation result, never reports success, and invalidates presence status

#### Scenario: Secret is absent from process metadata
- **WHEN** a native set is inspected across the helper descendant tree
- **THEN** the secret value is absent from every process argument and environment

### Requirement: Native operations are globally serialized
At most one native helper operation SHALL run across daemon and offline CLI processes for the configured state directory. Slot and lock acquisition SHALL be non-blocking with bounded retry until the caller deadline.

#### Scenario: Healthy operation owns the slot
- **WHEN** a second process attempts a native operation while a valid in-flight helper is running
- **THEN** the second process reports or retries `provider busy` without labeling the first helper wedged

#### Scenario: Lock acquisition expires
- **WHEN** the native sidecar lock remains held beyond the caller deadline
- **THEN** the operation returns a typed contention error and starts no helper

### Requirement: Native helper recovery proves process identity
mcpd SHALL record a restrictive, atomic, non-secret helper marker after process-tree isolation is confirmed and before it sends an operation request. Recovery SHALL signal only a helper whose executable, instance identifier, process start identity, and process group match the marker.

#### Scenario: Marker names an unrelated process
- **WHEN** a stale marker names a live same-user process that fails helper identity proof
- **THEN** mcpd removes the marker durably and sends no signal

#### Scenario: POSIX marker names the caller process group
- **WHEN** a marker process-group identifier matches the recovering process group
- **THEN** mcpd refuses the group signal and treats the marker as invalid

#### Scenario: Proven helper cannot be terminated
- **WHEN** bounded termination cannot confirm exit of an identity-proven helper tree
- **THEN** mcpd writes `phase=wedged`, releases the lock, blocks new native operations, and later revalidates with bounded backoff

#### Scenario: Wedged helper later exits
- **WHEN** background revalidation proves that the recorded helper is gone
- **THEN** mcpd removes the marker, clears wedged health, invalidates presence state, and requeues pending consumers

### Requirement: Native platform support passes adoption gates
Native support SHALL be enabled on a platform only after executable verification of bounded execution, process-tree termination, error categorization, metadata secrecy, atomic replacement, same-session non-interactive readback, and byte-exact round-trip fidelity.

#### Scenario: Adapter fails an adoption gate
- **WHEN** the selected native adapter fails any required platform test
- **THEN** mcpd leaves native support disabled on that platform or uses a compliant platform-specific adapter

#### Scenario: Existing credential is replaced atomically
- **WHEN** a helper is terminated at controlled points during replacement
- **THEN** the prior value remains readable unless the replacement completed atomically

### Requirement: macOS avoids recurring credential prompts
The macOS native adapter SHALL keep the release build free of cgo and SHALL use bounded `/usr/bin/security` operations. After an operation reaches `interaction-required`, automatic work SHALL not issue another value-bearing Keychain operation until explicit operator retry.

#### Scenario: Login keychain requires interaction
- **WHEN** a macOS native operation blocks on a locked keychain or authorization prompt
- **THEN** mcpd records `interaction-required`, leaves dependent consumers pending, and performs zero automatic value-bearing retries

#### Scenario: Operator retries after unlock
- **WHEN** the user unlocks the keychain and selects explicit retry
- **THEN** mcpd permits one bounded native attempt through the global slot

### Requirement: Managed-file writes are atomic and durable
The file provider SHALL store one immutable snapshot in the validated state directory. A dedicated restrictive `secrets.lock` sidecar SHALL never be replaced. Read-modify-write operations SHALL hold its exclusive lock through durable replacement and parent-directory synchronization.

#### Scenario: First file write
- **WHEN** the data file does not exist
- **THEN** mcpd treats the store as empty and creates the first restricted snapshot without exposing secret bytes through permissive temporary-file modes

#### Scenario: Concurrent writers set different names
- **WHEN** two processes set different names concurrently
- **THEN** locking serializes the read-modify-write operations and the final snapshot contains both values

#### Scenario: Existing file is corrupt
- **WHEN** the data file is present but malformed, truncated, or undecodable
- **THEN** mcpd returns a hard provider failure and does not treat the store as empty or overwrite it

#### Scenario: Reader observes replacement
- **WHEN** a POSIX reader opens the data file during replacement
- **THEN** it reads one complete old or new snapshot

### Requirement: External file changes are detected safely
mcpd SHALL watch the state directory rather than the replaceable data-file inode and SHALL use a periodic metadata check as fallback. It SHALL suppress duplicate reconnects for its own writes without persisting or logging a content digest.

#### Scenario: External atomic replacement occurs
- **WHEN** another authorized process atomically replaces the managed-file snapshot
- **THEN** mcpd reloads the final valid contents and reconnects affected consumers

#### Scenario: Daemon observes its own write event
- **WHEN** the directory watcher receives the event caused by a daemon-side write
- **THEN** digest comparison suppresses a second targeted reconnect
