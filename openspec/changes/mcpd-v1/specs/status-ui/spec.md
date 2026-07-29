## ADDED Requirements

### Requirement: Per-backend status is visible

The daemon SHALL serve a status surface listing every declared backend with its state,
transport, tool count, last refresh time, authentication state and token expiry, and last
error. This exists because backends currently fail silently, and because agents driven
over ACP have no MCP status surface of their own.

#### Scenario: A down backend is visible with its cause

- **WHEN** a backend is unreachable
- **THEN** it appears in the status surface as down, with its error text, within one
  refresh cycle

#### Scenario: A disabled backend is distinguishable from a broken one

- **WHEN** a backend has been disabled by the user
- **THEN** it is shown as disabled rather than as failing

### Requirement: Per-backend actions

The status surface SHALL offer, per backend, authenticate for OAuth backends, enable and
disable, and reconnect. It SHALL offer reconnect-all and re-index-catalog globally.

#### Scenario: Re-index is available without a restart

- **WHEN** the user triggers a catalog re-index
- **THEN** every enabled backend is re-listed and the catalog is updated

### Requirement: Backends are declared in a file the daemon never writes

Backend declarations SHALL live in a user-owned configuration file that the daemon only
reads. Runtime state, including enable/disable overrides, SHALL be stored separately, so
the daemon never has a write path into a file the user hand-edits.

#### Scenario: Toggling a backend does not rewrite the user's config

- **WHEN** a backend is disabled from the status surface
- **THEN** the override is recorded in daemon state and the user's configuration file is
  unchanged

#### Scenario: Adding a backend is a single-file edit

- **WHEN** the user wants to add a backend
- **THEN** they edit one configuration file, and no per-client configuration is touched

### Requirement: Tool inspector

The status surface SHALL provide an inspector that lists a backend's tools, shows a
selected tool's schema, accepts JSON arguments, invokes the tool, and displays the raw
result. This is the debugging path for a backend that is connected but misbehaving.

#### Scenario: A tool can be invoked and its result read

- **WHEN** the user supplies arguments for a tool and invokes it
- **THEN** the raw result is displayed as escaped text

#### Scenario: Destructive tools are called out

- **WHEN** a tool carries a destructive or absent read-only annotation
- **THEN** the inspector surfaces that and requires a second confirming action

#### Scenario: The confirmation is not relied on as a control

- **WHEN** the invoke route is reached directly rather than through the interface
- **THEN** the request is still subject to the origin and method guards, because the
  confirmation is a protection against the user's own misclick and not a security control
