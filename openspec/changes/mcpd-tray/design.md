## Context

See `proposal.md` for motivation. The daemon already exposes a guarded loopback status API and backend reconnect and authorization actions. The web projection also contains the only current definition of which states need attention. The release is one `CGO_ENABLED=0` Go executable for Linux and macOS on amd64 and arm64, while the daemon itself must remain usable without a graphical session.

The tray introduces three constraints the web panel does not have: it must survive daemon loss, it must integrate with two native desktop mechanisms, and it must never execute provider-derived browser targets or backend-derived text unsafely.

## Goals / Non-Goals

**Goals:**

- Preserve the single executable and headless daemon while adding an independent graphical process.
- Make the web panel and tray use one repair classification.
- Keep network and native event work bounded, race-free, and testable without a live desktop.
- Make clean quit, unsupported desktop state, and process failure observably different to the session supervisors.
- Keep a failed platform dependency out of the repository and release graph.

**Non-Goals:**

- A general desktop application framework or reusable widget toolkit.
- A second daemon-control interface or service-manager abstraction.
- Native rendering of detailed backend errors; the dashboard remains that surface.
- Automatic installation, notifications, or behavior outside graphical login sessions.

## Decisions

### Run `mcpd tray` as a separate process from the same executable

Argument dispatch will add `tray` beside the existing subcommands after the native credential-helper dispatch. The tray reads neither declarations nor daemon state; it communicates only through the loopback API.

Embedding the icon in the daemon was rejected because the icon would disappear when it is most needed and would force graphical lifecycle requirements into headless operation. Separate Swift and Linux applications were rejected because they add two toolchains and distribution products without improving the API-driven behavior.

### Gate the native tray dependency before production work

The candidate dependency must first pass a temporary-module spike on real macOS and Linux sessions. The gate proves zero-CGO builds for all release targets, native icon and nested-menu updates, no macOS Dock icon, safe controller-driven updates, clean shutdown, and defined behavior when a Linux StatusNotifierItem host is absent or appears late.

The dependency is added to `go.mod` only after that gate. A failure reopens the dependency decision; it does not permit enabling CGO, dropping a target, or introducing native application projects. `getlantern/systray` is not the automatic fallback because its CGO requirements conflict with the existing release contract.

### Make recommended action additive API data

The backend status projection gains a typed optional `recommended_action` with `reconnect` and `authorize` values. One classification method will drive the JSON field, the web attention list, labels, and paths. The tray consumes that decision instead of duplicating backend state interpretation.

This is preferable to teaching the tray about OAuth and rendered health because duplicated logic would drift. A new tray-specific endpoint was rejected because the current status response already contains the rest of the required snapshot and another endpoint would create a second contract.

### Use a narrow loopback client with separate budgets

The tray client accepts only a validated loopback host and port and uses a dedicated transport without environment proxies. Status reads have a two-second budget. Actions have a forty-second budget, covering the server's existing reconnect and authorization waits. Response bodies are bounded, backend paths are URL-escaped, and mutating calls remain guarded JSON POSTs.

Using the default HTTP client was rejected because proxy environment behavior and one global timeout are both wrong for a fixed local peer with short polls and longer interactive authorization.

### Serialize controller state and keep native callbacks non-blocking

One controller path owns the latest complete snapshot, current icon/menu model, action-in-flight state, and last fixed action error. Native callbacks enqueue work; polling and action requests execute outside the UI event loop; their completions return through the controller. Duplicate activation for a backend with an action in flight is disabled or ignored.

The tray polls every five seconds, at startup, after manual retry, and after action completion. A failed poll replaces the complete menu with an offline model rather than merging partial state. Actions are never automatically replayed.

Allowing callbacks and polls to mutate menu objects independently was rejected because a dynamic backend set and simultaneous action completion would make stale items and data races likely.

### Keep the menu small and keep untrusted errors out

Three embedded monochrome icons represent healthy, attention, and offline. The menu shows a fixed serving summary, top-level repair items, an all-servers submenu, dashboard and retry entries, and tray quit. Only validated backend names and mcpd-authored labels appear. Raw upstream errors and response bodies stay in the guarded dashboard.

Putting every backend at the top level was rejected because large installations bury the repair actions. Desktop notifications were rejected for the first version because the icon and action menu satisfy the visibility goal without adding permissions and transition-noise policy.

### Validate browser targets before platform opening

Authorization targets are parsed and allowed only for HTTPS or loopback HTTP. macOS invokes `/usr/bin/open`; Linux invokes `xdg-open`; both receive the URL as one argument without a shell. An unsafe or unopenable target leaves the backend actionable and points the user to the dashboard.

Trusting the provider-derived string because it came through the local daemon was rejected. The existing browser UI already applies the same scheme boundary before navigation.

### Give tray supervisors outcome-aware restart behavior

The Linux unit is tied to `graphical-session.target`, restarts only on failure after five seconds, and applies a bounded start limit. The macOS LaunchAgent is limited to the Aqua session, uses unsuccessful-exit-only keepalive, and throttles crash restarts. Exact launchd keys will be checked against the installed `launchd.plist(5)` documentation.

User quit, ordinary termination, and a distinctly reported unsupported/no-host desktop outcome exit successfully. Unexpected initialization and runtime failures exit non-zero. When the dependency supports late host registration, the process stays alive instead of exiting.

Using unconditional restart was rejected because it would undo the menu's Quit action and could turn a common GNOME missing-extension state into a restart loop. The tray definitions do not source login profiles or inherit daemon credentials because the tray needs only loopback HTTP and the desktop session.

## Risks / Trade-offs

- **The zero-CGO tray dependency is young or incomplete** -> prove every required native behavior before adding it to the module graph; stop on failure.
- **Linux indicator hosting varies by desktop** -> test host-present, host-absent, and host-appears-late cases; document the GNOME extension boundary.
- **CI can compile but cannot prove a visible native icon** -> require real-session acceptance on both platforms before claiming support.
- **A double activation could submit two repairs** -> track action-in-flight state and disable or ignore duplicate callbacks.
- **A poll can finish after a newer action refresh** -> serialize completions and replace state only through the controller's current generation.
- **launchd restart semantics are easy to misstate** -> verify the keepalive, successful-exit, Aqua-session, and throttle keys against the target host before committing.
- **A clean no-host exit will not notice an extension enabled later in the same session** -> prefer dependency-supported late registration; otherwise document the one manual tray restart.

## Migration Plan

1. Add the status field without removing or renaming existing JSON data.
2. Ship tray mode and startup files disabled by default.
3. Document manual execution and opt-in session startup.
4. Roll back by disabling/removing the tray startup file and reverting the tray dependency and command; daemon state, configuration, endpoints, and OAuth grants require no migration.
