## Purpose

Defines how mcpd resolves allowlisted credential references without changing legacy environment-only behavior or blocking unrelated consumers.

## ADDED Requirements

### Requirement: Secret resolution is opt-in
mcpd SHALL use provider-backed secret resolution only when the configuration contains a valid `secrets` block. If the block is absent, mcpd SHALL preserve the current environment-only expansion behavior exactly.

#### Scenario: Legacy configuration remains environment-only
- **WHEN** the `secrets` block is absent and a referenced environment variable is absent
- **THEN** mcpd logs the existing missing-variable warning, expands the reference to an empty string, and does not probe a credential provider

#### Scenario: Present empty environment value wins
- **WHEN** the process environment contains a referenced variable with an empty value
- **THEN** mcpd uses the empty value and does not query the configured provider

### Requirement: Environment values have priority
For an allowlisted reference, mcpd SHALL use the process environment value when the variable is present. It SHALL query the selected provider only when the variable is absent.

#### Scenario: Environment shadows a stored secret
- **WHEN** a referenced variable exists in the process environment and the provider also contains that name
- **THEN** mcpd uses the environment value and does not read the provider value

#### Scenario: Provider supplies an absent environment variable
- **WHEN** a referenced variable is absent from the process environment and present in the configured provider
- **THEN** mcpd uses the provider value for the dependent consumer

### Requirement: Provider resolution is allowlisted by consumer
mcpd SHALL permit provider lookup only for references in backend environment values, backend HTTP headers, and the embeddings API-key field. Secret-provider configuration and all other fields SHALL use literal or environment-only semantics.

#### Scenario: Non-allowlisted reference does not reach the provider
- **WHEN** a `${NAME}` reference appears outside an allowlisted consumer field
- **THEN** mcpd performs zero provider calls for that reference

#### Scenario: Provider construction cannot depend on itself
- **WHEN** the `secrets` block contains text that resembles a variable reference
- **THEN** mcpd parses it without querying the selected provider

### Requirement: Clean misses preserve existing behavior
A provider not-found result SHALL be distinct from provider failure. A clean miss SHALL produce the existing missing-variable warning and an empty expansion.

#### Scenario: Healthy provider does not contain a referenced name
- **WHEN** provider health succeeds and lookup reports that the exact name is absent
- **THEN** mcpd warns that the variable is missing and supplies an empty string to the consumer

#### Scenario: Provider failure is not reported as a clean miss
- **WHEN** lookup cannot determine presence because the provider is locked, denied, unavailable, timed out, wedged, corrupt, or otherwise failed
- **THEN** mcpd records a typed provider condition and does not report the name as absent

### Requirement: Resolution is atomic per consumer
mcpd SHALL resolve all provider-backed names required by one backend or the embeddings client as one consumer group. It SHALL construct or reconnect the consumer only after the complete group resolves or cleanly misses.

#### Scenario: Partial group resolution fails
- **WHEN** one name resolves and a later name in the same consumer group fails
- **THEN** mcpd discards the temporary resolved values, queues the complete group, and does not construct the consumer from partial data

#### Scenario: Pending group is retried as a complete group
- **WHEN** a previously partial group becomes eligible for background retry
- **THEN** mcpd resolves every required provider-backed name again before constructing the consumer

### Requirement: Startup is bounded and failure-isolated
mcpd SHALL apply an aggregate deadline to startup secret resolution. A provider condition SHALL affect only consumers with unresolved provider-backed references, while environment-satisfied and unrelated consumers remain available.

#### Scenario: Startup budget expires
- **WHEN** secret resolution exceeds the aggregate startup deadline
- **THEN** daemon startup completes, affected consumers report `pending secret resolution`, and unresolved groups remain queued

#### Scenario: One credential-dependent backend is blocked
- **WHEN** the provider is unavailable for one backend and another backend has no unresolved provider references
- **THEN** the unrelated backend remains operational

#### Scenario: Provider health short-circuits later groups
- **WHEN** a provider-level condition is established during startup
- **THEN** mcpd queues later provider-dependent groups without starting one helper per referenced name

### Requirement: Pending consumers recover through bounded work
mcpd SHALL process pending consumer groups serially in the background. Busy contention SHALL use a separate paced retry schedule and SHALL not become cached provider failure.

#### Scenario: Provider recovers non-interactively
- **WHEN** a Linux or Windows provider-health check later succeeds
- **THEN** mcpd requeues pending groups and connects each group after full resolution without a daemon restart

#### Scenario: Provider is busy
- **WHEN** another process holds the global native-operation slot
- **THEN** the consumer remains pending, retries with bounded contention backoff, and does not latch `busy` as provider health

### Requirement: Secret changes reconnect exact consumers
After a confirmed set or delete, mcpd SHALL invalidate applicable presence state and reconnect only backends and embeddings state that reference the changed name. Mutation locks SHALL be released before reconnect begins.

#### Scenario: Backend secret changes
- **WHEN** a secret used by one backend is set through the daemon
- **THEN** mcpd releases provider locks and performs one targeted reconnect for that backend without reconnecting unrelated backends

#### Scenario: Environment shadow cannot change through reconnect
- **WHEN** a newly stored secret is shadowed by a present daemon environment variable
- **THEN** mcpd retains the environment value and reports that removing it requires a daemon restart before the stored value can take effect
