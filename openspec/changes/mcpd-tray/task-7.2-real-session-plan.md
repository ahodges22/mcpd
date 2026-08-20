# Tray Real-Session Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete and sign off the macOS and Linux graphical-session matrix for visible state, native actions, browser launch, daemon recovery, clean Quit, crash restart, login start, and platform host behavior, then run one final adversarial review of the complete tray feature.

**Architecture:** Drive the production `mcpd tray` binary against a deterministic loopback acceptance API rather than the daily daemon. The fixture exposes healthy, reconnect, authorize, and unreachable states; counts actions; can hold reconnect to test duplicate suppression; and records loopback browser requests. Run the real macOS LaunchAgent in the current Aqua session and the real Linux systemd user unit in a disposable X11/systemd container, activate their native menu callbacks, and record exact bounded evidence in `verification.md`.

**Tech Stack:** Go 1.26.6, macOS 26.5.2 Aqua/launchd/Accessibility, Ubuntu 24.04 arm64, systemd 255, Xvfb, Openbox, XFCE StatusNotifier host, D-Bus/dbusmenu, `xdg-open`, Docker, OpenSpec.

**Spec:** `openspec/changes/mcpd-tray/specs/desktop-status/spec.md` and `openspec/changes/mcpd-tray/design.md`

## Scope / Out of scope

**In scope:**

- Exercise healthy, attention, offline, and recovery states in real native sessions on macOS and Linux.
- Activate reconnect twice while the first request is held and prove exactly one request reaches the API.
- Activate authorize and dashboard and prove the production platform opener reaches the intended loopback URL as one argument.
- Activate the actual Quit menu item and prove each supervisor leaves exit zero stopped.
- Prove login-scoped start and unsuccessful-exit restart on both platforms.
- Prove no macOS Dock application, Linux no-host survival, and Linux late-host registration.
- Review the cumulative tray feature diff from base commit `60a5cf3` with Grok and record the verdict.

**Out of scope:**

- Using the daily daemon, declarations, OAuth grants, backend credentials, or installed `~/.local/bin/mcpd`.
- Contacting a real OAuth provider or completing a real grant. Browser launch targets a controlled loopback page.
- Installing either tray supervisor permanently.
- Changing production Go, startup definitions, or release configuration unless execution exposes a verified defect.
- Capturing the whole macOS screen or closing existing browser tabs.

## Global Constraints

- Use fixed isolated macOS ports `47430` for the tray API and `47431` for control/browser recording; require both to be unused before starting.
- Use a unique macOS job label `dev.mcpd.tray.task72-probe` and a validated `/tmp/mcpd-task72.*` directory. Never replace the installed binary or plist.
- The macOS production opener will create two loopback-only tabs in the user's default browser. Do not manipulate or close any pre-existing tab.
- Crop macOS screenshots to the exact Accessibility-reported status-item bounds; never capture the full screen.
- The Linux probe runs only in a disposable privileged Ubuntu container with no host home, config, state, credential, browser, display, or session-socket mounts.
- Use the final zero-CGO Darwin and Linux arm64 binaries.
- Every control wait is condition-based with an explicit timeout. Do not treat a fixed sleep alone as proof.
- Preserve exact counters, PIDs, elapsed times, native menu labels, icon hashes or crops, supervisor properties, browser targets, tool versions, and cleanup results.

---

### Task 1: Build and self-check a disposable acceptance API

**Files:**
- Create temporarily: `/tmp/mcpd-task72-acceptance.go`
- Remove after both platform runs: `/tmp/mcpd-task72-acceptance.go`

**Interfaces:**
- Consumes: `--api-addr`, `--control-addr`, and a process context.
- Produces: deterministic tray API responses plus isolated control and browser-observation endpoints.

- [x] **Step 1: Implement the exact fixture contract with the Go standard library**

The temporary command owns two listeners and one mutex-protected state object. The API listener exposes:

```text
GET  /api/status
GET  /
POST /api/backends/broken/reconnect
POST /api/backends/oauth/authorize
```

The control listener exposes:

```text
POST /control/reset
POST /control/mode/healthy
POST /control/mode/reconnect
POST /control/mode/authorize
POST /control/reconnect/hold
POST /control/reconnect/release
POST /control/api/stop
POST /control/api/start
GET  /control/counters
GET  /oauth-approved
```

`GET /api/status` returns exactly one of these snapshots:

```json
{"backends":[{"name":"healthy","state":"up","label":"serving"}],"serving":1}
{"backends":[{"name":"broken","state":"down","label":"unavailable","recommended_action":"reconnect"}],"serving":0}
{"backends":[{"name":"oauth","state":"needs_auth","label":"needs authorization","recommended_action":"authorize"}],"serving":0}
```

Reconnect locks only long enough to increment `reconnect_calls` and capture the current hold/release channel, then unlocks before waiting. It must never retain the state mutex across a channel wait or network write. After release it locks again, changes the mode to healthy, unlocks, and returns `{}`. Authorize increments `authorize_calls`, changes the mode to healthy, and returns `{"authorize_url":"http://<control-addr>/oauth-approved?source=tray"}`. API root increments `dashboard_calls`; `/oauth-approved` increments `oauth_page_calls`. Counters also include `status_calls`, API stop/start counts, and the last dashboard and OAuth request targets. Starting or stopping the API listener must not stop the control listener. Status, counters, and reconnect release must remain responsive while a reconnect request is held.

- [x] **Step 2: Self-check every state, action, hold, counter, and listener transition**

Run the fixture on unused temporary ports, then use `curl --fail` to require all three exact status documents and one blocked reconnect that does not complete before release. While that request remains blocked, require status to return the reconnect snapshot within one second, counters to report exactly one reconnect call within one second, and the release endpoint to answer within one second. After release, require the original request to complete and status to become healthy. Also require one authorize URL with the control listener's loopback address, dashboard and OAuth page increments, connection refusal while the API listener is stopped, and healthy status after restart. Stop the fixture and require both listeners to refuse connections.

Expected: every predicate passes without accessing mcpd, a browser, or a graphical session.

---

### Task 2: Complete the macOS Aqua and LaunchAgent matrix

**Files:**
- Create temporarily: one validated `/tmp/mcpd-task72.*` directory containing the zero-CGO binary, fixture binary, modified probe plist, logs, and status-item crops.
- Modify after success: `openspec/changes/mcpd-tray/verification.md`

**Interfaces:**
- Consumes: final `dist/dev.mcpd.tray.plist`, the final Darwin arm64 binary, acceptance API at `127.0.0.1:47430`, control/browser listener at `127.0.0.1:47431`, and the current `gui/<uid>` Aqua domain.
- Produces: a complete macOS matrix without touching the installed mcpd binary, daemon, config, state, credentials, or LaunchAgents directory.

- [x] **Step 1: Bootstrap the exact supervisor semantics under a unique label**

Require ports 47430 and 47431 to be unused. Build both final zero-CGO binaries into a validated temporary directory. Copy `dist/dev.mcpd.tray.plist`, change only its label and command's binary path/address, and lint it. Start the fixture, then bootstrap the plist into `gui/$(id -u)`.

Require launchd to report a non-zero user-owned PID without `launchctl kickstart`, proving implied login-session start. Require `lsappinfo info -only kLSApplicationTypeKey <pid>` to report `UIElement`, and require no application of type `Foreground` for that PID.

- [x] **Step 2: Add bounded Accessibility helpers for native inspection and activation**

Use `osascript` with `System Events` and `first application process whose unix id is <pid>`. Locate the status item by finding the menu-bar item whose click exposes menu labels containing `Open dashboard` and `Quit status icon`. Helpers must:

```text
list-menu <pid>                         -> newline-delimited native labels
click-menu <pid> <exact label>         -> activate one native item
status-item-bounds <pid>                -> x,y,width,height
```

If Accessibility is denied or no status item can be located, stop and report this as a critical host blocker. Do not use coordinate guessing. Use the reported bounds with `screencapture -x -R` so each crop contains only the mcpd item.

- [x] **Step 3: Prove healthy, attention, duplicate reconnect, and recovery**

Require the initial native menu to contain `1 of 1 backends serving`, `healthy - serving`, `Open dashboard`, `Refresh status`, and `Quit status icon`, then capture the healthy item crop.

Set reconnect mode and hold reconnect. Condition-wait until the menu contains `Reconnect broken`, `0 of 1 backends serving`, and `broken - unavailable`, then capture the attention crop. Activate `Reconnect broken`, reopen the menu while the request is held, and attempt the same activation again. Require `reconnect_calls=1`. Release the request and require healthy menu recovery with no second reconnect.

- [x] **Step 4: Prove authorize and dashboard browser launch**

Set authorize mode and wait for `Authorize oauth`. Activate it once and require `authorize_calls=1`, `oauth_page_calls=1`, and the exact target `/oauth-approved?source=tray`. Activate `Open dashboard` and require `dashboard_calls=1` with the exact API root URL. These actions deliberately open two loopback-only tabs through `/usr/bin/open`; do not close or modify browser tabs afterward.

- [x] **Step 5: Prove unreachable state and automatic daemon recovery**

Stop only the fixture API listener. Condition-wait until the native menu contains `mcpd is unreachable`, `Retry now`, and `Quit status icon`, then capture the offline item crop. Restart the API listener in healthy mode and require the healthy menu to return within seven seconds without restarting the tray process. Require the healthy, attention, and offline crops to be non-empty and pairwise different.

- [x] **Step 6: Prove actual Quit and crash behavior under launchd**

Activate `Quit status icon` through Accessibility. Wait longer than `ThrottleInterval` and require the loaded job to have no PID, `state = not running`, and `last exit code = 0`.

Record a monotonic timestamp immediately before starting it again with `launchctl kickstart -p`, condition-wait past the root-owned `xpcproxy` handoff, and require that handoff to the user-owned process within one second. SIGKILL immediately after the handoff. Require a different PID to appear under the same job no sooner than nine seconds after the original kickstart timestamp. If the handoff exceeds one second, discard that timing sample and restart the isolated crash probe rather than letting a slow bootstrap make the throttle check pass. End the replacement with the actual Quit menu item and require it to remain stopped.

- [x] **Step 7: Record and clean up macOS evidence**

Append a macOS matrix table to `verification.md` with every scenario, command/observation, exact result, PID, timing, menu label set, crop hash, browser target, and supervisor property. Boot out only the unique probe, stop the fixture, remove only the validated temporary directory, and confirm both ports, the job label, fixture process, and temporary directory are absent. Record that the daily daemon PID and listener were unchanged.

---

### Task 3: Complete the Linux graphical-session and systemd matrix

**Files:**
- Create temporarily: `/tmp/mcpd-task72-linux-systemd.Dockerfile`
- Modify after success: `openspec/changes/mcpd-tray/verification.md`

**Interfaces:**
- Consumes: final Linux arm64 binary, final `dist/mcpd-tray.service`, the acceptance fixture, and a disposable privileged Ubuntu 24.04 arm64 container.
- Produces: a complete Linux X11/StatusNotifier/systemd matrix with no host user data mounted.

- [x] **Step 1: Prepare the disposable graphical user session**

Create one validated temporary Docker build-context directory containing the Dockerfile, zero-CGO Linux arm64 mcpd binary, compiled fixture binary or fixture source, final unit, and helper scripts before invoking `docker build`; do not expect a repo-root build context to read `/tmp`. Build a temporary image with systemd 255, D-Bus, Xvfb, Openbox, XFCE panel and StatusNotifier plugin, `gdbus`, Python D-Bus bindings, `xdg-utils`, `curl`, and screenshot utilities. Create unprivileged user `probe`, install the final binary at `/home/probe/.local/bin/mcpd`, install the unchanged final unit under its user config, and install the fixture on fixed loopback ports 7420 and 7421. The final unit's `%h/.local/bin/mcpd tray` command therefore resolves to the staged probe binary, and its default address resolves to the fixture at `127.0.0.1:7420`; do not add a drop-in or change the production unit.

Create a disposable desktop handler whose command records its one URL argument by requesting that loopback URL, register it as the default HTTP/HTTPS handler, and leave `/usr/bin/xdg-open` unchanged. This proves the production code invoked real `xdg-open` while preventing an external browser from escaping the container.

- [x] **Step 2: Prove session-target login start, host absence, and late host arrival**

Boot the container with systemd and start the `probe` user manager. Use only its standard `%t/bus`; do not launch a second session bus. As `probe`, set and import `DISPLAY=:91`, `XDG_RUNTIME_DIR=/run/user/<uid>`, `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus`, `XDG_CURRENT_DESKTOP=XFCE`, and `XDG_SESSION_TYPE=x11` into that user manager before starting any graphical process or target. Start the fixture, Xvfb, and Openbox as `probe`; start Openbox and later xfce4-panel through user-manager units so they inherit that exact environment. Enable `mcpd-tray.service`. Activate `graphical-session.target` indirectly through a disposable session target that wants it, matching a desktop session manager rather than manually starting the passive target.

Before accepting the login start, inspect the loaded unit's resolved `ExecStart` and require `/home/probe/.local/bin/mcpd tray` with no non-default address, then require the fixture's `status_calls` to increase. Inspect `/proc/<pid>/environ` without printing it and require the tray and XFCE panel to use the same user-manager bus address, runtime directory, display, and UID. Treat any split bus, user, runtime directory, display, binary path, or API target as a harness failure.

Before starting XFCE panel, require the tray MainPID to stay active for more than two poll cycles and the journal to contain the distinct missing-watcher warning. Start the real XFCE StatusNotifier host and require the same PID to register an item without another setter call or process restart.

- [x] **Step 3: Add exact D-Bus menu inspection and activation helpers**

Run the Python helper as `probe` with the imported user-manager bus environment. Use Python D-Bus bindings to locate the registered `org.kde.StatusNotifierItem-*` service on that exact bus, read its `/StatusNotifierItem` `Menu` and `IconPixmap` properties, call `com.canonical.dbusmenu.GetLayout`, recursively map exact labels to item IDs, and call `Event(id, "clicked", variant, timestamp)` for activation. Refuse zero or multiple exact-label matches or any bus address other than `unix:path=/run/user/<uid>/bus`.

Record the IconPixmap digest and complete native menu labels for every state. Use the real panel item bounds to capture only the mcpd indicator, not the full desktop.

- [x] **Step 4: Prove healthy, attention, duplicate reconnect, and recovery**

Repeat the macOS healthy and reconnect sequence through the real D-Bus menu: require the exact healthy labels, switch to held reconnect, require the attention labels and changed icon digest, activate `Reconnect broken` twice while held, require `reconnect_calls=1`, release, and require healthy recovery.

- [x] **Step 5: Prove authorize and dashboard through real `xdg-open`**

Switch to authorize, activate `Authorize oauth`, and require one authorize request plus one `/oauth-approved?source=tray` browser-recorder request. Activate `Open dashboard` and require one API-root recorder request. Inspect the recorder arguments and require each URL arrived as exactly one argument.

- [x] **Step 6: Prove unreachable state and automatic daemon recovery**

Stop only the fixture API listener. Require the offline menu, changed IconPixmap digest, and process survival. Restart the listener healthy and require the healthy menu and original icon digest within seven seconds without a tray PID change.

- [x] **Step 7: Prove actual Quit and crash behavior under systemd**

Read the loaded unit's normalized restart delay from `systemctl --user show mcpd-tray.service -p RestartUSec` and require it to equal the value declared by `RestartSec` in `dist/mcpd-tray.service`. Activate `Quit status icon` through dbusmenu, wait longer than that loaded delay, and require `ActiveState=inactive`, `SubState=dead`, `Result=success`, and no restart. Start the service again, SIGKILL MainPID, and require a different PID no sooner than the loaded `RestartUSec` floor. End the replacement through the actual Quit menu item and require it to remain stopped.

- [x] **Step 8: Record and clean up Linux evidence**

Append the Linux matrix table to `verification.md` with exact versions, menu labels, icon digests/crop hashes, action counters, browser arguments, PIDs, timing, unit properties, watcher registration, and journal evidence. Reset any failed unit, then remove the disposable container and image plus the explicit temporary Dockerfile. Confirm no container, image tag, process, or temporary file remains.

---

### Task 4: Final gate, cumulative Grok review, task state, and commit

**Files:**
- Modify: `openspec/changes/mcpd-tray/verification.md`
- Modify: `openspec/changes/mcpd-tray/tasks.md`
- Modify: `openspec/changes/mcpd-tray/task-7.2-real-session-plan.md`

**Interfaces:**
- Consumes: the signed-off two-platform matrix and cumulative tray commits after `60a5cf3`.
- Produces: completed OpenSpec change and commit `test: complete tray acceptance review`.

- [x] **Step 1: Run the final automated gate**

Run the same complete automated gate recorded in Task 7.1 from the final source tree, including the race suite, vet, vulnerability scan, plist lint, four-archive GoReleaser snapshot, exact nine-member and checksum assertions, strict OpenSpec validation, and diff checks.

- [x] **Step 2: Submit the cumulative implementation to Grok adversarial code review**

Run branch-mode review against base `60a5cf3` with repository context and self-collection. Scope the review to the changed production, test, startup, release, and user-guide paths while excluding the separately verified 626 KB notices file and generated binary assets:

```text
cmd/mcpd/main.go cmd/mcpd/main_test.go cmd/mcpd/tray.go cmd/mcpd/tray_test.go cmd/mcpd/tray_service_test.go internal/tray internal/web/page.go internal/web/page_test.go go.mod go.sum .goreleaser.yaml .github/workflows/ci.yml dist/mcpd-tray.service dist/dev.mcpd.tray.plist dist/README.md README.md
```

Require review of lifecycle/thread ownership, HTTP boundaries, action serialization, browser safety, native adapter semantics, supervisor outcome classification, environment isolation, packaging, documentation, and whether the real-session evidence supports the platform claims. Resolve every merge-blocking finding and rerun all affected automated and real-session checks after any production edit.

- [x] **Step 3: Record review, mark Task 7.2, and commit**

Append the Grok verdict and any finding ledger to `verification.md`. After all matrix cells pass and no material finding remains, mark only Task 7.2 complete, check this plan's steps, remove reproducible `.release` output and every remaining Task 7.2 temporary artifact, and run:

```bash
git commit -m "test: complete tray acceptance review"
```

Expected: the OpenSpec change reports 16 of 16 tasks complete, strict validation passes, and the worktree is clean.
