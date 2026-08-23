# desktop-status Specification

## Purpose
Provide an optional graphical-session status companion that keeps MCP backend failures visible and puts the correct repair action within one menu interaction on Linux and macOS.

## Requirements

### Requirement: The tray is independent from the daemon
The system SHALL run the tray as a separate process from the daemon, while distributing both modes in the same `mcpd` executable. The tray SHALL remain available when the daemon cannot be reached and SHALL NOT start, stop, or restart the daemon.

#### Scenario: Daemon failure remains visible
- **WHEN** a completed status poll cannot reach the daemon
- **THEN** the tray remains running, changes to its offline icon and menu, and offers retry and tray-quit actions without offering backend repair actions

#### Scenario: Daemon recovery requires no tray restart
- **WHEN** the daemon becomes reachable after the tray entered its offline state
- **THEN** the tray restores the current backend status and icon within one five-second poll cycle

### Requirement: The tray summarizes and lists backend status
The tray SHALL show the serving count and total declared backend count, SHALL list every backend in an `All servers` submenu, and SHALL lift each actionable backend into a one-click repair item above that submenu. Backend-provided error text SHALL NOT be placed in native menu labels.

#### Scenario: A backend needing attention is immediately actionable
- **WHEN** the daemon reports a recommended action for a backend
- **THEN** the tray uses the attention icon and shows a top-level action naming that backend and the reported repair

#### Scenario: Neutral states do not raise attention
- **WHEN** a backend is healthy, disabled, or has never been dialled and has no actual fault
- **THEN** it remains visible in the all-servers list but does not change the tray to its attention state or receive a repair item

#### Scenario: A large backend set stays navigable
- **WHEN** more backends exist than are practical to show at the menu top level
- **THEN** only actionable entries remain at the top and every backend remains available in the all-servers submenu

### Requirement: Attention classification has one machine-readable source
The status API SHALL emit an optional `recommended_action` value of `reconnect` or `authorize` for each actionable backend. The web panel and tray SHALL consume that same classification so they cannot recommend different repairs for the same snapshot.

#### Scenario: OAuth authorization is recommended
- **WHEN** a backend needs authorization, or an OAuth-declared backend has an actual connection fault
- **THEN** its status entry contains `recommended_action: "authorize"` and both status surfaces offer authorization

#### Scenario: Reconnect is recommended
- **WHEN** a non-OAuth backend has an actual connection fault
- **THEN** its status entry contains `recommended_action: "reconnect"` and both status surfaces offer reconnect

#### Scenario: No repair is recommended
- **WHEN** a backend is healthy, disabled, or not yet dialled without an error
- **THEN** its status entry omits `recommended_action`

### Requirement: Tray repair actions are bounded and not replayed
The tray SHALL submit the existing guarded JSON action for the selected backend at most once per user activation, SHALL ignore or disable duplicate activation while that action is active, and SHALL refresh status when the action finishes. It SHALL NOT automatically replay an action after an error or daemon restart.

#### Scenario: Reconnect is submitted once
- **WHEN** the user activates reconnect for one backend multiple times before the first request finishes
- **THEN** exactly one reconnect request is in flight and the tray refreshes status after it completes

#### Scenario: Action outcome becomes unknown
- **WHEN** the daemon disconnects after a tray action was submitted
- **THEN** the tray reports a bounded failure, does not replay the action, and relies on the next status poll

#### Scenario: Action failure stays diagnosable
- **WHEN** a backend action fails
- **THEN** the backend remains actionable and the tray shows a fixed failure note directing the user to the dashboard for details

### Requirement: Authorization opens only an approved browser target
The tray SHALL open an authorization target only when it is HTTPS, or HTTP with a loopback hostname. It SHALL pass the target as one process argument rather than through a shell.

#### Scenario: Provider returns an unsafe authorization URL
- **WHEN** the authorize response contains a malformed URL or a scheme outside the allowlist
- **THEN** the tray refuses to open it, keeps the backend actionable, and directs the user to the dashboard

#### Scenario: Provider returns an approved authorization URL
- **WHEN** the authorize response contains an HTTPS URL or loopback HTTP URL
- **THEN** the tray opens it with the platform's default browser and refreshes backend status after the action

### Requirement: Tray startup is optional and session-scoped
The release SHALL include opt-in Linux and macOS startup definitions that start `mcpd tray` only with a graphical login session. Installing or enabling the tray SHALL remain a separate user action from supervising the daemon.

#### Scenario: User quits the tray
- **WHEN** the user selects `Quit status icon`
- **THEN** the tray exits successfully and its supervisor does not restart it in the same session

#### Scenario: Tray crashes
- **WHEN** the tray exits unexpectedly
- **THEN** its session supervisor restarts it with a bounded delay and rate limit without affecting the daemon

#### Scenario: Linux has no status notifier host
- **WHEN** the tray starts in a graphical session without a StatusNotifierItem host
- **THEN** it either remains alive until a host appears or exits through a distinct successful unsupported-environment outcome, and it does not enter a supervisor restart loop

### Requirement: Supported releases preserve the single-binary contract
Linux and macOS release archives SHALL contain the same `mcpd` executable with daemon and tray modes, SHALL preserve `CGO_ENABLED=0`, and SHALL include tray startup definitions and documentation. Platform support SHALL NOT be claimed until icon, menu, action, quit, crash-restart, and daemon recovery behavior have passed in real graphical sessions on both operating systems.

#### Scenario: Release artifacts are built
- **WHEN** a release snapshot is produced
- **THEN** all Linux and macOS amd64 and arm64 archives build without CGO and contain both tray startup definitions

#### Scenario: Stock GNOME lacks an indicator host
- **WHEN** the Linux documentation describes GNOME compatibility
- **THEN** it names the StatusNotifierItem extension requirement and does not claim the icon is visible without a compatible host
