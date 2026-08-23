# backend-oauth Specification

## Purpose
Define browser-initiated OAuth authorization, callback integrity, restrictive token persistence, refresh behavior, and provider compatibility for proxied backends.

## Requirements

### Requirement: OAuth-gated backends are usable through the proxy

A backend declaring OAuth authentication and carrying no static credential SHALL be
reachable through the daemon. The daemon SHALL act as the OAuth client: discovering
authorization server metadata, registering a client, obtaining a grant, persisting the
result, and refreshing it.

#### Scenario: An unauthenticated backend surfaces as needing auth

- **WHEN** an OAuth backend has no stored token
- **THEN** its state is `needs-auth` and an authorization URL is available for the user

### Requirement: Authorization is triggered from the browser, not the daemon

The daemon SHALL NOT attempt to open a browser itself, because a systemd user service has
no reliable session bus. It SHALL record the authorization URL and expose it, so the user
initiates the flow from the status page.

#### Scenario: The user starts the flow from the status page

- **WHEN** the user activates the authenticate action for a backend needing auth
- **THEN** the authorization URL opens in the user's browser and the daemon awaits the
  callback

### Requirement: The callback is bound to one pending authorization by a one-time state

The redirect URI SHALL be a fixed loopback address, because it is registered with the
provider and must remain stable. The callback SHALL validate its `state` parameter against
the pending-authorization registry before any token exchange, SHALL require it to match
exactly one outstanding authorization, and SHALL consume it on use.

#### Scenario: A forged callback is rejected

- **WHEN** a callback arrives with a `state` matching no outstanding authorization
- **THEN** no token exchange occurs and no token is written

#### Scenario: A replayed callback is rejected

- **WHEN** a callback that has already been consumed is replayed
- **THEN** it matches no outstanding authorization and is rejected

#### Scenario: The callback is reached as a browser navigation

- **WHEN** the provider redirects the browser to the callback as a top-level GET with a
  query string
- **THEN** the request is accepted, because this route is the single documented exemption
  from the POST-only and JSON-only mutation rules

### Requirement: Tokens are persisted with restrictive permissions

Tokens and any registered client credentials SHALL be written to per-backend files at
mode 0600, inside a directory at mode 0700. A refreshed token SHALL be written back, so a
restart reuses it rather than re-authorizing.

A record SHALL also persist the declaration identity it was issued under, and that identity
SHALL be compared against the current declaration before the record authenticates anything. A
record is keyed only by backend name, and the name says nothing about which provider issued the
token or under what declaration, so a repointed backend would otherwise present a token to an
endpoint it was not issued for. A record whose identity does not match, including one that
carries no identity at all, is unusable: it is discarded, deleted, and the backend reports
`needs-auth`. A refresh SHALL NOT persist a record for a backend that is no longer declared.

The comparison SHALL happen where the authorization handler is obtained, not only where a record
is read. One handler is cached per backend name and returned without rereading, and it owns a live
token source holding the token in memory, so a check on the record read alone would never run once
a handler is primed. Deleting a record SHALL therefore also discard that backend's cached handler,
its token source and any pending authorization.

The `backend-management` capability covers what changes an identity, why deleting the record at
the moment of change is not sufficient on its own, and why the override store resolves the same
ambiguity in the opposite direction.

#### Scenario: A repointed declaration does not reuse its token

- **WHEN** a backend's declared URL, auth mode or transport differs from the identity its
  stored record was issued under
- **THEN** the record is discarded and deleted, the token is never sent to the provider, and
  the backend reports `needs-auth`

#### Scenario: A restart reuses the stored token

- **WHEN** the daemon is restarted after a successful authorization
- **THEN** the backend reconnects authenticated with no further user interaction

#### Scenario: An expired access token is refreshed

- **WHEN** an access token has expired and a refresh token is held
- **THEN** the token is refreshed transparently rather than returning the backend to
  `needs-auth`

#### Scenario: A failed refresh stops rather than looping

- **WHEN** a refresh attempt fails
- **THEN** the backend returns to `needs-auth` rather than retrying indefinitely

### Requirement: Provider compatibility is established before it is depended upon

Because provider-specific behaviour cannot be proven by a fake provider, the metadata
discovery, client registration, and loopback-redirect assumptions SHALL be verified
against the real provider before the surrounding implementation depends on them, and the
findings SHALL be recorded.

#### Scenario: Loopback redirect registration is verified

- **WHEN** a dynamic client registration is attempted with the fixed loopback redirect URI
- **THEN** the provider's acceptance or rejection is recorded, and a rejection is treated
  as invalidating the fixed-port design rather than as a detail to work around
