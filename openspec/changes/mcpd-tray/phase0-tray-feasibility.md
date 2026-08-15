# Phase 0 Tray Feasibility

## Dependency evaluation

Status: build gate passed on 2026-08-14. Runtime acceptance remains pending on macOS and Linux, so the dependency is not yet approved for `go.mod`.

- Candidate: `github.com/gogpu/systray v0.2.8`
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

- [ ] Renders one `NSStatusItem` in a real Aqua session.
- [ ] Does not create a Dock icon.
- [ ] Replaces healthy, attention, and offline icons while running.
- [ ] Updates a nested backend menu while running.
- [ ] Keeps the native callback responsive while work runs asynchronously.
- [ ] Removes the icon and exits cleanly.

## Linux graphical-session acceptance

- [ ] Renders one StatusNotifierItem with a host present.
- [ ] Replaces healthy, attention, and offline icons while running.
- [ ] Updates a nested backend menu while running.
- [ ] Keeps the native callback responsive while work runs asynchronously.
- [ ] Reports a distinct unsupported outcome when no host is present.
- [ ] Registers successfully when a host appears after process startup, or documents the required manual restart.
- [ ] Removes the icon and exits cleanly.

## Supervisor acceptance

These checks belong to the later startup tasks.

- [ ] Linux clean Quit does not restart the tray.
- [ ] Linux unexpected failure restarts after a delay and remains start-limited.
- [ ] macOS clean Quit does not restart the tray.
- [ ] macOS unsuccessful exit restarts after throttling.
