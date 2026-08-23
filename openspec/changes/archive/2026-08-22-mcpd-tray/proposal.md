## Why

Backend and OAuth failures are visible only after a user notices missing tools and opens the
loopback panel to investigate. Daemon supervision repairs a dead process, but it does not make
an unhealthy backend visible or put its repair action where the user will see it.

## What Changes

- Add an `mcpd tray` mode that runs as a separate graphical-session process from the existing
  binary and reports daemon and backend health in the macOS menu bar or Linux status area.
- Give the tray three states: healthy, backend attention required, and daemon unreachable.
- List every backend in the tray menu, lift actionable failures to the top, and offer exactly
  one contextual repair action for each: reconnect or authorize.
- Extend the status projection with a machine-readable recommended action shared by the web
  panel and tray, so the two surfaces cannot classify the same backend differently.
- Ship opt-in graphical-session startup definitions for Linux and macOS, independently of the
  existing daemon supervisor definitions.
- Gate the tray dependency on real macOS and Linux validation, and document that stock GNOME
  commonly needs a StatusNotifierItem-compatible extension.

The first version does not send desktop notifications, provide a global fix-all action, treat
disabled or never-dialled backends as failures, or start, stop, or restart the daemon.

## Capabilities

### New Capabilities

- `desktop-status`: graphical-session tray lifecycle, status and attention presentation,
  contextual repair actions, daemon-offline behavior, and opt-in startup on Linux and macOS.

### Modified Capabilities

None.

## Impact

- **Command surface:** the existing binary gains the `mcpd tray` mode.
- **Status API:** backend status gains a machine-readable recommended action without changing
  the loopback trust boundary or existing action routes.
- **Runtime:** an optional second process runs only in a graphical login session; the daemon
  remains independently supervised and continues to support headless use.
- **Distribution:** release archives remain single-binary, with additional Linux and macOS
  startup definitions and tray icon assets.
- **Dependency:** a cross-platform Go tray library is expected, but adoption is conditional on
  dynamic icon, dynamic menu, browser launch, daemon-loss, and recovery tests on real macOS and
  Linux sessions.
- **Linux compatibility:** StatusNotifierItem-compatible desktops are supported; environments
  without a host, including stock GNOME without a suitable extension, receive documentation
  rather than an alternate native implementation.
