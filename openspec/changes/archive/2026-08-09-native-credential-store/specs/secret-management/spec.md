## Purpose

Defines write-only local management of allowlisted secrets through the mcpd CLI and loopback panel without disclosing stored values.

## ADDED Requirements

### Requirement: Secret management is write-only
The CLI and local web panel SHALL support set, status, retry, and remove operations. No operation, response, log, diagnostic, HTML document, or browser state SHALL return a stored secret value.

#### Scenario: Status is requested
- **WHEN** a user requests secret status through the CLI or panel
- **THEN** mcpd returns names, consumers, effective sources, and typed conditions only

#### Scenario: Secret is set through the panel
- **WHEN** the panel submits a valid secret value
- **THEN** the response confirms the mutation without echoing the value

### Requirement: Status is derived from allowlisted references
Secret status SHALL include only exact names referenced by the loaded configuration in provider-allowlisted consumer fields. It SHALL include referenced missing names and SHALL not enumerate unreferenced provider entries.

#### Scenario: Provider contains an unreferenced entry
- **WHEN** a credential exists under the mcpd service namespace but no allowlisted configuration field references it
- **THEN** ordinary status omits the entry

#### Scenario: Environment satisfies a referenced name
- **WHEN** a referenced name is present in the daemon environment
- **THEN** status reports `environment; provider not checked` and performs no provider probe

### Requirement: Presence status does not retain values
mcpd SHALL cache only present, absent, or typed-condition state for a configurable interval with a five-minute default. A cached presence result SHALL never substitute for a value needed to construct a consumer.

#### Scenario: Panel polls within the cache interval
- **WHEN** status is requested repeatedly for one provider-backed name inside the cache interval
- **THEN** mcpd performs at most one provider lookup and never stores the returned value in the status cache

#### Scenario: Operator requests refresh
- **WHEN** a user explicitly refreshes secret status
- **THEN** mcpd probes through the normal global slot, deadline, and provider-health rules

### Requirement: CLI set input avoids process arguments
`mcpd secret set` SHALL read a value from a hidden terminal prompt or standard input and SHALL not accept the value as a command-line argument.

#### Scenario: Terminal input is used
- **WHEN** the user enters one line at the hidden prompt
- **THEN** mcpd removes the line terminator only and validates the remaining bytes

#### Scenario: Standard input is used
- **WHEN** stdin supplies bytes to EOF
- **THEN** mcpd removes at most one final LF or one final CRLF and performs no other trimming or normalization

### Requirement: CLI uses the daemon when available
The CLI SHALL attempt the daemon local API first so a successful mutation and targeted reconnect form one daemon-coordinated operation.

#### Scenario: Daemon accepts the mutation
- **WHEN** the local daemon is available
- **THEN** the CLI does not open the provider directly and reports the daemon result

#### Scenario: Daemon is unavailable
- **WHEN** the local API cannot be reached
- **THEN** the CLI may use the selected provider directly after state-directory and identity validation, then makes one best-effort local notification

### Requirement: Offline access matches the daemon identity
Before offline provider mutation, the CLI SHALL require its effective user to match the owner identity of an existing state directory. A newly created state directory SHALL establish and report the invoking identity as the expected daemon identity.

#### Scenario: Existing owner does not match
- **WHEN** an offline CLI invocation uses an identity other than the validated state-directory owner
- **THEN** mcpd refuses before provider mutation and identifies both identities without exposing credential data

#### Scenario: First offline setup
- **WHEN** the configured state directory does not exist
- **THEN** mcpd creates it with restrictive platform permissions, validates it, and reports the identity that now owns daemon state

### Requirement: Native offline diagnostics preserve session context
Offline native operations SHALL identify the current credential namespace and require the intended user's unlocked login session. Diagnostics SHALL not recommend `sudo -u` as a substitute for macOS Keychain or Linux session D-Bus context.

#### Scenario: Offline native set succeeds
- **WHEN** a same-session user writes a native secret while the daemon is stopped
- **THEN** the CLI names the credential identity and states that environment shadowing cannot be determined until daemon startup

#### Scenario: Service account lacks session storage
- **WHEN** a headless POSIX identity has no usable native credential session
- **THEN** the CLI reports the limitation and points to explicit file-provider configuration without selecting it automatically

### Requirement: Environment shadowing is visible at mutation time
A daemon-side set SHALL inspect the effective source. If the name is present in the daemon environment, mcpd SHALL store the new value but return a prominent warning that the environment continues to win.

#### Scenario: Stored value is shadowed
- **WHEN** a set succeeds for a name that exists in the daemon environment
- **THEN** the CLI and panel report success plus the restart-required shadow warning without probing or returning the provider value

### Requirement: Removal reports and refreshes dependents
Before removal, mcpd SHALL identify configured consumers that reference the name. After confirmed deletion, it SHALL invalidate presence state and reconnect those consumers after releasing mutation locks.

#### Scenario: Referenced secret is removed
- **WHEN** the user confirms removal of a name used by multiple consumers
- **THEN** mcpd reports the dependent consumer names and reconnects only those consumers after deletion

### Requirement: Web management remains local and origin-protected
Secret-management routes SHALL use the existing loopback and same-origin protections. They SHALL not be exposed on the remote relogin surface.

#### Scenario: Cross-origin mutation is attempted
- **WHEN** a request to set or remove a secret fails the shared origin guard
- **THEN** mcpd rejects the request before reading or mutating provider state

#### Scenario: Remote surface is inspected
- **WHEN** a client uses the remote relogin listener
- **THEN** secret-management routes and values are absent
