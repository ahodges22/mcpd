# Phase 0 Tray Feasibility

## Dependency evaluation

Status: build and platform runtime gates passed. `github.com/ahodges22/systray` is the approved maintained source for the required macOS main-thread patch; production pinning remains blocked only on verifying and immutably pinning that fork.

- Candidate: `github.com/gogpu/systray v0.2.8`
- Approved patched source: `github.com/ahodges22/systray`, aligned with upstream and limited to the verified main-thread patch
- Module checksum: `h1:3C/jUnHGO/e/kCbrRWw9n3psuuP7pfvSer68sqjzA4U=`
- Declared Go version: `1.25.0`, compatible with mcpd's `1.26.5`
- License: MIT
- Platform approach: `NSStatusItem` through `goffi` on macOS and StatusNotifierItem through D-Bus on Linux

The complete candidate module graph is:

| Module | Version | Relationship | License |
| --- | --- | --- | --- |
| `github.com/gogpu/systray` | `v0.2.8` | candidate | MIT |
| `github.com/go-webgpu/goffi` | `v0.6.3` | macOS transitive dependency | MIT |
| `github.com/godbus/dbus/v5` | `v5.2.2` | Linux transitive dependency | BSD-2-Clause |
| `golang.org/x/sys` | `v0.47.0` | candidate module graph; already a direct mcpd dependency | BSD-3-Clause |

The macOS binaries embed `goffi`; the Linux binaries embed `godbus/dbus`. `go version -m` records `CGO_ENABLED=0` for every binary.

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

## Supervisor acceptance

These checks belong to the later startup tasks.

- [ ] Linux clean Quit does not restart the tray.
- [ ] Linux unexpected failure restarts after a delay and remains start-limited.
- [ ] macOS clean Quit does not restart the tray.
- [ ] macOS unsuccessful exit restarts after throttling.
