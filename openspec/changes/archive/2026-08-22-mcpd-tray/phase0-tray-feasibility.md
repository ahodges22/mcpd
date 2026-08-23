# Phase 0 Tray Feasibility

## Dependency evaluation

Status: build and platform runtime gates passed. `github.com/ahodges22/systray` is the approved maintained source for the required macOS main-thread runtime-mutation patch and is pinned to the independently reviewed immutable revision below.

- Candidate: `github.com/gogpu/systray v0.2.8`
- Approved patched source: `github.com/ahodges22/systray`, aligned with upstream and limited to the main-thread dispatcher, Linux close lifecycle, and error plumbing required for runtime icon and complete-menu replacement
- Pinned fork commit: `8d1612f5113230275c23f80236b7f2690da54af7`
- Pinned pseudo-version: `v0.2.9-0.20260819052144-8d1612f51132`
- Pinned module checksum: `h1:WXsTBAcGL9XmmfSV58f1446twJr87A3NEK9Mtt3EH6E=`
- Pinned `go.mod` checksum: `h1:UTsyHG33eGgLuQcvwBfaKo7wuLtnRloMh0WZjVGdFp0=`
- Upstream base: `github.com/gogpu/systray@a3901e26a16407483bcb765d35cba446e60c6932` (`v0.2.9-0.20260812082930-a3901e26a164`)
- Declared dependency Go version: `1.25.0`, compatible with mcpd's `1.26.6`
- License: MIT; the pinned source has no `NOTICE`
- Platform approach: `NSStatusItem` through `goffi` on macOS and StatusNotifierItem through D-Bus on Linux

The complete candidate module graph is:

| Module | Version | Relationship | License |
| --- | --- | --- | --- |
| `github.com/ahodges22/systray` | `v0.2.9-0.20260819052144-8d1612f51132` | pinned patched dependency | MIT |
| `github.com/gogpu/systray` | `v0.2.8` | original candidate only, not in production graph | MIT |
| `github.com/go-webgpu/goffi` | `v0.6.3` | macOS transitive dependency | MIT |
| `github.com/godbus/dbus/v5` | `v5.2.2` | Linux transitive dependency | BSD-2-Clause |
| `golang.org/x/sys` | `v0.47.0` | candidate module graph; already a direct mcpd dependency | BSD-3-Clause |

The macOS binaries embed `goffi`; the Linux binaries embed `godbus/dbus`. `go version -m` records `CGO_ENABLED=0` for every binary.

The pinned graph was resolved without a `replace` directive. `go mod graph` reports the fork's direct edges to `goffi v0.6.3`, `godbus/dbus v5.2.2`, and `x/sys v0.47.0`. The fork and `goffi` license artifacts were read from the exact pinned module sources before their `LICENSE` and `NOTICE` text was added to `THIRD-PARTY-NOTICES`.

Fork verification before publication passed on macOS and Linux: full tests, race tests repeated five times, `go vet`, real `dbus-run-session` property/menu mutation tests repeated five times under the race detector, and zero-CGO builds for Linux and macOS on amd64 and arm64 plus unchanged Windows amd64 compatibility. Grok's independent Cursor review approved the complete uncommitted fork diff after three iterations and three addressed lifecycle findings. The published fork worktree was clean at the pinned commit.

## Reproduction spike

The temporary module used Go 1.26.5 and this program shape:

```go
package main

import "github.com/gogpu/systray"

func main() {
	menu := systray.NewMenu()
	menu.Add("Status", nil)

	backends := systray.NewMenu()
	backends.Add("example: connected", nil)
	menu.AddSubmenu("All servers", backends)

	tray := systray.New().
		SetIcon(nil).
		SetTemplateIcon(nil).
		SetTooltip("mcpd").
		SetMenu(menu).
		Show()
	if err := tray.Run(); err != nil {
		panic(err)
	}
}
```

## Zero-CGO build acceptance

All commands ran from the temporary module on macOS arm64. A successful `go build` writes no stdout or stderr, so the recorded output is the exit status and inspected file type.

| Command | Exit | Output inspection | SHA-256 |
| --- | ---: | --- | --- |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o build/linux-amd64 .` | 0 | ELF x86-64, statically linked | `e696cd5928de60173142b801a096aeb386870d8090d630f311ed906f820a65b6` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o build/linux-arm64 .` | 0 | ELF ARM aarch64, statically linked | `514d02c27ee5b66a4fbe2c0d8b25cfe0a2d5f653f8dd83e3bbb8d2d84aa8803a` |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o build/darwin-amd64 .` | 0 | Mach-O x86_64 | `3b2ffa75e58b136b1f35f984d257eca7f344cfdb6f585e842a1acde1b4fc8917` |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o build/darwin-arm64 .` | 0 | Mach-O arm64 | `b303bfb148e2e0a5d8e78665016dab8f443e8ac56ab106cdc8fa516d73959f6a` |

Result: the candidate preserves mcpd's zero-CGO, single-binary release constraint for all four release targets.

## macOS graphical-session acceptance

- [x] Renders one `NSStatusItem` in a real Aqua session.
- [x] Does not create a Dock icon.
- [x] Replaces healthy, attention, and offline icons while running.
- [x] Updates a nested backend menu while running.
- [x] Keeps the native callback responsive while work runs asynchronously.
- [x] Removes the icon and exits cleanly.

### macOS run evidence, 2026-08-15

- The unmodified candidate failed `TestSetTemplateIconMarshalsAppKitMutationToMainThread` with `probe_main_thread=false`. Its `SetTemplateIcon` method created `NSImage` and mutated `NSStatusBarButton` on the controller goroutine.
- A temporary patch reused the dependency's main-thread target to marshal runtime icon mutations. The regression test and `TestSetTemplateIconFromWorkerBeforeRunReturnsError` then passed, followed by `CGO_ENABLED=0 go test ./... -count=1`.
- Fresh patched builds passed for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` with `CGO_ENABLED=0`.
- The patched arm64 spike entered `Run` in the logged-in user's Aqua session. `lsappinfo` reported it as `ApplicationType="UIElement"`, confirming accessory activation without a Dock icon.
- Alex visually confirmed one status item cycling through the healthy ring, attention triangle, and offline X. Alex also confirmed the changing nested backend label and that the menu reopened immediately while the three-second action worker was active.
- Selecting `Quit feasibility spike` removed the visible item. The log recorded `quit-clicked`, `run-stopped`, and exit status 0.

Gate result: passed with a required narrow main-thread patch. Unmodified `github.com/gogpu/systray v0.2.8` remains rejected. Before production pinning, the patch must be available from an upstream release or an explicitly approved maintained source.

## Linux graphical-session acceptance

- [x] Renders one StatusNotifierItem with a host present.
- [x] Replaces healthy, attention, and offline icons while running.
- [x] Updates a nested backend menu while running.
- [x] Keeps the native callback responsive while work runs asynchronously.
- [x] Reports a distinct unsupported outcome when no host is present.
- [x] Registers successfully when a host appears after process startup, or documents the required manual restart.
- [x] Removes the icon and exits cleanly.

### Linux run evidence, 2026-08-15

- Environment: a disposable OrbStack container running Ubuntu 24.04.4 LTS on arm64, with Xvfb 21.1.12, Openbox 3.6.1, `xfce4-panel` 4.18.4, and `xfce4-sntray-plugin` 0.4.13.1. This was a real Linux X11 session with an actual StatusNotifier host, not a mocked D-Bus watcher. Physical-desktop coverage remains in Task 7.2.
- The patched zero-CGO Linux arm64 spike had SHA-256 `8b9e14253fd8a75eeeaba754a5c2627b6a134a284956bdc0ad2851bdba85f359`.
- With the XFCE host present, `org.kde.StatusNotifierWatcher.RegisteredStatusNotifierItems` contained exactly `org.kde.StatusNotifierItem-2957-2/StatusNotifierItem`. Captured 1024 by 768 screenshots showed the healthy ring, attention triangle, and offline X replacing one another in the panel.
- Opening the item showed the changing summary and the nested `All servers` menu. The nested label changed with state, including `example: unavailable` during offline state.
- Selecting `Run non-blocking probe` logged `callback-returned duration=62.748µs`. The three-second worker ran separately, and the icon and menu remained responsive while it was active.
- Selecting `Quit feasibility spike` logged `quit-clicked` and `run-stopped`. The watcher's registered-item array was empty afterward, and the post-Quit screenshot matched the no-item baseline.
- With the XFCE host stopped but the X session and session bus still running, startup logged one explicit warning: `initial watcher registration failed (watcher may not be running)`. The process remained alive, continued cycling states, and retained its `org.kde.StatusNotifierItem-3066-2` bus name. This is distinct from a generic process failure and does not enter a restart loop.
- Starting the XFCE host afterward registered `org.kde.StatusNotifierItem-3066-2/StatusNotifierItem` automatically. The process remained PID 3066 with its original start time, and the item rendered without a process restart or manual intervention.
- The exact disposable container was removed after the logs, D-Bus replies, screenshots, package versions, and hashes were captured.

Gate result: passed. Host absence is explicit and non-fatal, and late host appearance recovers automatically. No manual restart is required.

## Native adapter evidence, 2026-08-18

- `TestNativeAdapterLifecycle` passed 100 race-enabled repetitions. It covers synchronous offline apply, pre-ready latest-only coalescing, recoverable initial and runtime apply failures, already-cancelled and cancellation-during-initial-apply exits, update-channel closure, removal before join, blocked runtime apply release, and exact driver-error forwarding.
- `TestBuildNativeMenu`, `TestSystrayDriverApply`, and `TestTemplateIconAlphaMasksAreDistinct` passed. The three embedded icons have distinct alpha masks, so Darwin uses template icons. Complete menu snapshots preserve ordering, separators, disabled state, nested children, and value-captured callbacks. Identical menus are retained, changed menus are replaced, and failed menu/show calls retry on the next complete snapshot.
- `go test -count=1 -race ./...`, `go vet ./...`, `go vet ./internal/tray/testdata/darwin-session`, strict OpenSpec validation, and zero-CGO `mcpd` builds for Linux and Darwin on amd64 and arm64 passed with Go 1.26.6.
- The first vulnerability scan on Go 1.26.5 correctly failed on six reachable standard-library advisories fixed in 1.26.6. After raising the repository patch-level requirement, `go tool govulncheck ./...` reported zero reachable vulnerabilities and zero vulnerable imported packages. Three required modules contain vulnerabilities only in packages mcpd does not import.
- A real arm64 Aqua process built from `internal/tray/testdata/darwin-session` ran through multiple complete snapshots and automatic cancellation. `lsappinfo` reported the process as `type="UIElement"`, confirming no Dock application, and the native loop exited cleanly after the seven-second probe.
- A real arm64 Linux session-bus process stayed alive and continued updating with no StatusNotifier host, logging the expected distinct watcher warning. A second D-Bus connection read `Title='mcpd'`, `IconPixmap`, `Menu=/MenuBar`, and the exported dbusmenu layout. The process removed itself cleanly after automatic cancellation.
- With `DBUS_SESSION_BUS_ADDRESS` absent, the same Linux binary returned a synchronous construction error and never entered the native loop. Late-host registration, disabled initial item properties, live replacement, and remove-during-setter behavior also passed the pinned fork's real `dbus-run-session` race tests five times.
- Grok's independent Cursor review approved the complete mcpd Task 5.1 adapter diff. It verified the lifecycle contract, callback boundary, exact fork API usage, immutable pseudo-version and checksums, Go 1.26.6 requirement, and Darwin/Linux clean removal. The 626 KB `THIRD-PARTY-NOTICES` file was explicitly excluded from the model payload; its new fork and `goffi` sections were instead checked directly against the exact pinned `LICENSE` and `NOTICE` artifacts.

Exact automated commands:

```text
go test -race ./internal/tray -run '^TestNativeAdapterLifecycle$' -count=100
go test -count=1 -race ./...
go vet ./...
go vet ./internal/tray/testdata/darwin-session
go tool govulncheck ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/mcpd
openspec validate mcpd-tray --strict
git diff --check
```

## Tray command evidence, 2026-08-19

- The browser boundary and command lifecycle were implemented test-first. The initial focused runs failed because `openURLWith`, the dashboard controller action, `runTray`, and the startup dispatch did not exist. The same focused tests then passed under the race detector, including 20 repeated controller runs, 50 repeated command runs, and 10 combined `cmd/mcpd` and `internal/tray` runs.
- `OpenURL` accepted a metacharacter-rich valid HTTPS URL as exactly one process argument, selected `/usr/bin/open` on Darwin and `xdg-open` on Linux, rejected unsafe URLs and unsupported platforms before invocation, and honored a cancelled context.
- The complete repository race suite, both vet targets, all four zero-CGO release builds, strict OpenSpec validation, and `git diff --check` passed. `govulncheck` reported zero reachable vulnerabilities and zero vulnerable imported packages; three required modules contain vulnerabilities only in packages mcpd does not import.
- A final arm64 macOS binary ran the daemon from isolated temporary config and state at `127.0.0.1:47420`, then ran `mcpd tray` in the logged-in Aqua session. `lsappinfo` reported `ApplicationType="UIElement"` for tray PID 32646. SIGTERM removed the process registration and returned status 0. The daemon also shut down cleanly.
- The same macOS binary rejected `--addr 0.0.0.0:7420` with an invalid-loopback-address error and status 1 before registering an application.
- The final zero-CGO Linux arm64 binary ran in an ephemeral Ubuntu 24.04 arm64 container under `dbus-run-session` with no StatusNotifier watcher. It stayed alive, owned an `org.kde.StatusNotifierItem-*` bus name, emitted the expected distinct watcher-registration warning, and returned status 0 after SIGTERM.
- In a second clean Linux session bus, `--addr 0.0.0.0:7420` returned the invalid-loopback-address error and no `org.kde.StatusNotifierItem-*` name appeared.
- Grok's independent Cursor review approved the full uncommitted Task 5.2 change set on iteration 1 with no skipped files and no merge-blocking findings. It confirmed credential-helper precedence, startup-thread ownership, callback boundaries, browser argument safety, loopback validation, exit classification, and controller shutdown. It identified only two non-blocking UX tradeoffs: the intentionally bounded shared action queue can drop a click while busy, and a foreground `xdg-open` implementation remains attached until completion or context cancellation.

Exact final automated commands:

```text
go test -race ./internal/tray -run 'Test(ControllerDashboardIsNonBlocking|ControllerSerializesAction|ControllerActionFailure|ControllerDoesNotReplay|ControllerActionShutdown|OpenURLArgumentBoundary)$' -count=20
go test -race ./cmd/mcpd -run '^TestRunTray(Flags|RejectsNonLoopback|ExitOutcome)$' -count=50
go test ./cmd/mcpd -run '^Test(MainLocksStartupThreadBeforeDispatch|CredentialHelperDispatchPrecedesTray)$' -count=1
go test -race ./cmd/mcpd ./internal/tray -count=10
go test -count=1 -race ./...
go vet ./...
go vet ./internal/tray/testdata/darwin-session
go tool govulncheck ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/mcpd-linux-amd64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/mcpd-linux-arm64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o /tmp/mcpd-darwin-amd64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/mcpd-darwin-arm64 ./cmd/mcpd
openspec validate mcpd-tray --strict
git diff --check
```

## Linux tray supervisor evidence, 2026-08-19

- The final zero-CGO Linux arm64 binary and `dist/mcpd-tray.service` ran under a real systemd 255.4 user manager in a disposable privileged Ubuntu 24.04 arm64 container. The container mounted no user home, configuration, state, credential, or host session sockets.
- `systemd-analyze --user verify /home/probe/.config/systemd/user/mcpd-tray.service` returned 0 with no output. The unit was enabled only under `graphical-session.target.wants`; the supervisor probe started the service directly because `graphical-session.target` is passive and has `RefuseManualStart=yes`.
- With no StatusNotifier watcher, the process remained `active/running` for more than two five-second poll cycles and logged the distinct expected watcher-registration warning.
- The user manager contained synthetic `MCPD_PROBE_SECRET=must-not-pass` and `DISPLAY=:91`. `/proc/<MainPID>/environ` contained the allowlisted display value and did not contain the synthetic secret, proving the `/usr/bin/env -i` boundary rather than a credential-name denylist.
- Direct SIGTERM produced `Result=success`, `ActiveState=inactive`, `SubState=dead`, `MainPID=0`, and `NRestarts=0` after seven seconds. Combined with `TestRunTrayExitOutcome` proving the Quit callback reaches the same successful cancellation path, clean Quit remains stopped.
- Direct SIGKILL changed PID 260 to PID 803 after 5,139 ms and returned to `ActiveState=active`, proving the five-second restart floor. Repeated SIGKILLs within the sixty-second window produced three active replacements, then no PID and a stable failed unit. systemd 255 retained `Result=signal`, incremented the restart counter to 4, and logged `Start request repeated too quickly`, proving the three-start rate limit even though this version does not expose `start-limit-hit` as the final `Result` value.
- The failed unit was reset, then the disposable container and locally tagged test image were removed. No service, user, image tag, or mounted host data remained.
- Grok's independent Cursor review approved the complete Task 6.1 diff on iteration 1 with no blocking findings. It confirmed the graphical-session lifecycle, clean-success classification, five-second restart delay, three-start rate limit, and synthetic-secret exclusion. Its non-blocking browser-path and installation notes remain assigned to Tasks 7.2 and 6.3 respectively.

Exact focused commands and outcomes:

```text
go test ./cmd/mcpd -run '^TestLinuxTrayService$' -count=1
systemd-analyze --user verify /home/probe/.config/systemd/user/mcpd-tray.service
systemctl --user show mcpd-tray.service -p ActiveState -p SubState -p MainPID -p NRestarts -p Result
```

## macOS tray supervisor evidence, 2026-08-19

- The final `CGO_ENABLED=0` Darwin arm64 binary ran as a real LaunchAgent in the current Aqua session on macOS 26.5.2 build 25F84. The installed supervisor was Darwin Bootstrapper 7.0.0 from `libxpc_executables-3102.120.13~112`.
- `plutil -lint dist/dev.mcpd.tray.plist` returned `OK`. The installed `launchd.plist(5)` documentation confirmed that `LimitLoadToSessionType` applies to agents, `SuccessfulExit=false` restarts the inverse of exit status zero and implies initial launch, and `ThrottleInterval` limits spawn frequency in seconds.
- The test copied the final plist to a validated `/tmp/mcpd-task62.*` directory and changed only its label, binary path, and loopback address. It used unique label `dev.mcpd.tray.task62-probe` and verified that `127.0.0.1:47429` had no listener. The normal daemon listening on `127.0.0.1:7420` was not stopped, restarted, or modified.
- Initial PID 74008 remained alive for eleven seconds while its configured daemon address was offline. `lsappinfo` classified it as `ApplicationType=UIElement`, confirming Aqua registration without a Dock application.
- The GUI launchd environment contained synthetic `MCPD_TASK62_PROBE_SECRET=must-not-pass`, set with `launchctl setenv`. A quiet `ps eww` predicate confirmed that the long-running tray process contained the allowlisted `HOME` and did not contain the synthetic secret after the non-login shell executed `/usr/bin/env -i`.
- `TestRunTrayExitOutcome` passed immediately before direct SIGTERM. PID 74008 then exited with `last exit code = 0`; the loaded job reported `state = not running`, `runs = 1`, and no PID after more than the ten-second throttle interval, proving the successful Quit path remains stopped.
- A prompt SIGKILL of PID 74855 produced replacement PID 75933 after a 10,239 ms spawn-to-spawn interval. Promptly killing PID 75933 produced PID 76509 after 10,052 ms. Both intervals exceeded the nine-second observation floor around the documented ten-second throttle, proving unsuccessful exits restart without a spin loop.
- `launchctl kickstart -p` briefly reports a root-owned `xpcproxy` before that same PID executes as the user job. The probe condition-waited for the user-owned process before signaling it, preventing the harness from mistaking the bootstrap transition for tray behavior.
- Cleanup waited for asynchronous `launchctl bootout` completion, unset only the synthetic variable, and removed only the validated temporary directory. Final checks found no probe label, synthetic variable, or Task 6.2 directory; the normal daemon remained listening on its original PID and port.
- Grok's independent Cursor plan review approved the Task 6.2 implementation plan on iteration 1. Its three non-blocking precision notes were incorporated: each crash interval has its own timestamp, the existing Quit exit-zero test is composed with the real signal probe, and the synthetic secret enters through `launchctl setenv` rather than a shell export.
- Grok's independent Cursor code review approved the complete Task 6.2 diff on iteration 1 with no skipped files or blocking findings. It confirmed the successful Quit classification, unsuccessful-exit restart, Aqua scope, ten-second spawn throttle, non-login `env -i` boundary, and live synthetic-secret evidence. It agreed that packaging, installation guidance, and browser click-through remain in Tasks 6.3 and 7.2.

Exact focused commands and outcomes:

```text
go test ./cmd/mcpd -run '^TestMacOSTrayLaunchAgent$' -count=1
plutil -lint dist/dev.mcpd.tray.plist
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <validated-temp-dir>/mcpd ./cmd/mcpd
go test ./cmd/mcpd -run '^TestRunTrayExitOutcome$' -count=1
launchctl print gui/501/dev.mcpd.tray.task62-probe
```

## Supervisor acceptance

These checks belong to the later startup tasks.

- [x] Linux clean Quit does not restart the tray.
- [x] Linux unexpected failure restarts after a delay and remains start-limited.
- [x] macOS clean Quit does not restart the tray.
- [x] macOS unsuccessful exit restarts after throttling.
