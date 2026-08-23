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
disable, reconnect, and remove. It SHALL offer reconnect-all, re-index-catalog, add-backend,
and reload-declarations globally.

#### Scenario: Re-index is available without a restart

- **WHEN** the user triggers a catalog re-index
- **THEN** every enabled backend is re-listed and the catalog is updated

### Requirement: Backends are declared in one file, editable two ways

Backend declarations SHALL live in a single configuration file, editable by hand or from the
status surface. Runtime state, including enable and disable overrides, SHALL be stored
separately, so a runtime toggle never rewrites a declaration.

The `backend-management` capability governs the daemon's write path into that file,
including how a concurrent hand edit is protected.

#### Scenario: Toggling a backend does not rewrite the declaration file

- **WHEN** a backend is disabled from the status surface
- **THEN** the override is recorded in daemon state and the configuration file is unchanged

#### Scenario: Adding a backend touches no per-client configuration

- **WHEN** the user adds a backend, by editing the file or from the status surface
- **THEN** one configuration file changes, and no per-client configuration is touched

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
