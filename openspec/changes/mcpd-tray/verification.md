# Tray Verification

## Automated gate, 2026-08-19

The complete automated gate ran from clean commit
`ee07e1c56f139254847688356ef9c5dd69176979` on macOS 26.5.2 arm64 with
`go version go1.26.6 darwin/arm64`.

Exact commands:

```bash
go test -count=1 -race ./...
go vet ./...
go tool govulncheck ./...
plutil -lint dist/dev.mcpd.tray.plist
go run github.com/goreleaser/goreleaser/v2@v2.17.1 release --snapshot --clean --skip=sign

test "$(find .release -maxdepth 1 -name 'mcpd_*_*.tar.gz' | wc -l | tr -d ' ')" -eq 4
for archive in .release/mcpd_*_*.tar.gz; do
  archive_contents="$(tar -tzf "$archive")"
  test "$(wc -l <<<"$archive_contents" | tr -d ' ')" -eq 9
  for member in mcpd README.md LICENSE THIRD-PARTY-NOTICES dist/README.md dist/mcpd.service dist/mcpd-tray.service dist/dev.mcpd.daemon.plist dist/dev.mcpd.tray.plist; do
    grep -qx "$member" <<<"$archive_contents"
  done
done
(cd .release && shasum -a 256 -c checksums.txt)

openspec validate mcpd-tray --strict
git diff --check
git status --short
```

Results:

- The race-enabled Go suite passed for every package. Packages without tests were reported as such; no test failed or was skipped because of an error.
- `go vet ./...` returned 0 with no findings.
- `govulncheck` found zero reachable vulnerabilities and zero vulnerabilities in imported packages. It found three advisories in required modules only in packages mcpd does not call.
- `plutil` reported `dist/dev.mcpd.tray.plist: OK`.
- GoReleaser v2.17.1 built Linux and Darwin archives for amd64 and arm64 with the repository's `CGO_ENABLED=0` build configuration. The snapshot version was `0.2.1-SNAPSHOT-ee07e1c`.
- Exactly four archives were produced. Every archive contained exactly these nine members: `mcpd`, `README.md`, `LICENSE`, `THIRD-PARTY-NOTICES`, `dist/README.md`, `dist/mcpd.service`, `dist/mcpd-tray.service`, `dist/dev.mcpd.daemon.plist`, and `dist/dev.mcpd.tray.plist`.
- All four entries in `.release/checksums.txt` passed SHA-256 verification.
- Strict OpenSpec validation reported `Change 'mcpd-tray' is valid`.
- `git diff --check` returned 0, and `git status --short` produced no output before this evidence file was created.

## Coverage boundary

This gate proves automated behavior, race safety, static plist coverage, zero-CGO release builds, and archive contents. The complete visible menu, action click-through, browser launch, login-start, daemon-recovery, and cross-platform graphical-session matrix remains Task 7.2 and is not claimed here. Linux systemd parser and supervisor behavior and macOS launchd supervisor behavior were already exercised under Tasks 6.1 and 6.2 and are recorded in `phase0-tray-feasibility.md`; Task 7.1 did not repeat those destructive supervisor probes.

## Real-session acceptance, 2026-08-20

The production `mcpd tray` command ran against a standard-library-only loopback fixture with deterministic healthy, attention, authorization, and unreachable states. The final fixture source SHA-256 was `21e97204ca58f80834b41fd18a9e9418abfa15c8eb63a03a0d16c152ae28b40d`. Its held reconnect copied the release channel under its mutex and released the mutex before waiting; status, counters, and release remained responsive while the request was held. A real-browser check exposed and corrected one fixture-only issue: the root handler now rejects `/favicon.ico` instead of counting it as a dashboard open.

### macOS Aqua and LaunchAgent matrix

Environment: macOS 26.5.2 arm64, Aqua domain `gui/501`, production zero-CGO binary SHA-256 `db614b28ecd06c111e16ad04bbae9ef1a17a8c43f64ab640ae6c1d942fc66a29`, unique job `dev.mcpd.tray.task72-probe`, and isolated listeners `127.0.0.1:47430` and `127.0.0.1:47431`. The probe plist changed only the label, binary path, and loopback address from `dist/dev.mcpd.tray.plist`; its SHA-256 was `255cc962d5329701cafceb5280b6b205d034ddd0ed5dab0fb8b676dfa9785b00` and `plutil -lint` passed.

| Scenario | Real-session result |
| --- | --- |
| Login-scoped start and host type | `launchctl bootstrap gui/501` started user-owned PID 67141 without `kickstart`. `lsappinfo` reported `ApplicationType=UIElement`; no foreground/Dock application existed for the PID. |
| Visible healthy state | Accessibility exposed exactly one 40 by 24 status item. After revealing the user's Thaw hidden section it was on-screen at `1134,3,40,24`. Native labels were `1 of 1 backends serving`, `All servers`, child `healthy - serving`, `Open dashboard`, `Refresh status`, and `Quit status icon`. |
| Menu-bar manager boundary | Thaw was configured with `NewItemsSection=hidden`, so it initially relocated this new item to `-3981,3,40,24`, outside both active displays. The same binary in a normal foreground shell had the same placement. Temporarily invoking Thaw's reversible `toggle-hidden` action moved it on-screen, proving this was the user's menu-bar policy rather than mcpd, launchd, or the systray fork. |
| Attention and duplicate reconnect | Native labels changed to `0 of 1 backends serving`, `Reconnect broken`, `All servers`, child `broken - unavailable`, and the common rows. Two real Accessibility activations while the first request was held produced `reconnect_calls=1`; release restored the exact healthy menu with no second request. |
| Authorization | Native `Authorize oauth` activation produced `authorize_calls=1`, `oauth_page_calls=1`, and exact target `/oauth-approved?source=tray`. The production `/usr/bin/open` path opened the controlled loopback page. |
| Dashboard | Native `Open dashboard` activation produced `dashboard_calls=1` and exact target `/`. A browser favicon request was rejected and did not affect the counter. |
| Daemon loss and recovery | Stopping only the API listener changed the native menu to `mcpd is unreachable`, `Open dashboard`, `Retry now`, and `Quit status icon` in 2.232 seconds. The on-screen item was `1051,1283,40,24` on the second display. Restarting the API restored healthy state in 4.636 seconds with PID 74833 unchanged. |
| Clean Quit | Activating the actual `Quit status icon` left the loaded job `state = not running`, without a PID, with `last exit code = 0`. It remained stopped after 11 seconds, longer than the 10-second throttle interval. |
| Crash restart | `launchctl kickstart -p` handed off to user-owned PID 76532 in 0.146 seconds. SIGKILL produced replacement PID 76855 at 10.625 seconds from kickstart, satisfying the nine-second floor. Activating the replacement's actual Quit item again left exit zero stopped after 11 seconds. |
| Daily daemon isolation | The installed daemon remained PID 56303 and retained its `127.0.0.1:7420` listener before and after the matrix. Its binary, configuration, state, credentials, and installed LaunchAgents were not touched. |

Accessibility and every native action were available. Region capture was the one host limitation: macOS TCC reported `The user declined TCCs for application, window, display capture` for responsible application `com.t3tools.t3code`, even for a one-pixel probe. No full-screen capture was attempted, and no macOS crop hash is claimed. On-screen Accessibility bounds, exact native labels, real click-through counters, and the three distinct menu models provide the macOS visibility and state evidence.

The unique job was booted out, both listeners refused connections, the validated temporary directory and helper were removed, and no Task 7.2 macOS process remained.

### Linux X11, XFCE StatusNotifier, and systemd matrix

Environment: a privileged but host-unmounted Ubuntu 24.04 arm64 container with systemd `255.4-1ubuntu8.17`, D-Bus `1.14.10-4ubuntu4.1`, Xvfb `21.1.12-1ubuntu1.6`, Openbox `3.6.1-12build5`, XFCE panel `4.18.4-1ubuntu0.1`, and `xfce4-sntray-plugin` `0.4.13.1-2build3`. Docker reported no mounts. The disposable image digest was `sha256:8da05d90e9d190e0c864b71dca4519ca73bd36a4b9781b7255049afef322c979`. The final Linux mcpd binary SHA-256 was `9786b16024d0661dac3f58814d8b14a9b7a77769e01aca227c17d8e900b7689d`; the Linux fixture binary was `d6cebe8ceb5c78637c5359c307625280ff403aa6d7319df2257688265e7d53c5`; the unchanged unit was `ee268b746f2ee5c307e01ee29b4ef4bdcd58cccd7be858c90bbbced571ea9fd5`.

| Scenario | Real-session result |
| --- | --- |
| Exact production unit and login start | Loaded `ExecStart` resolved to `/home/probe/.local/bin/mcpd tray` with no address override, so it reached the fixture at the default `127.0.0.1:7420`. Enabling the unchanged unit and starting a disposable session target that wanted `graphical-session.target` started tray PID 213 and incremented `status_calls`; the passive graphical target was not manually started. |
| One session bus and environment | Tray PID 213 and panel PID 291 were UID 1001 and had exact `DISPLAY=:91`, `XDG_RUNTIME_DIR=/run/user/1001`, `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus`, `XDG_CURRENT_DESKTOP=XFCE`, and `XDG_SESSION_TYPE=x11`. Exactly one user-bus socket existed. |
| Host absent | Before XFCE started, PID 213 survived 11 seconds and more than two poll cycles (`status_calls` 4 to 6). The journal contained `initial watcher registration failed (watcher may not be running)` and the watcher name was absent, distinguishing this from process failure. |
| Host appears late | Starting the real XFCE panel registered `org.kde.StatusNotifierItem-213-2/StatusNotifierItem`; tray PID 213 did not change and no additional setter or restart was needed. |
| Healthy native state | D-Bus exposed `Menu=/MenuBar`, service `org.kde.StatusNotifierItem-213-2`, and exact healthy labels matching macOS. `IconPixmap` SHA-256 was `8ef2ce356377ecf8ac2dcbcb044c67c06d500a2f9d635e2f17d236677e29c2a9`. The real panel item's exact 24 by 28 crop SHA-256 was `fbd206e605e7988d60f9790b18b80554b5ebe8fffef7e7a900ca1e3faef5c610`. |
| Attention and duplicate reconnect | Exact labels were `0 of 1 backends serving`, `Reconnect broken`, `All servers`, child `broken - unavailable`, and the common rows. `IconPixmap` was `ce945952f8d768d1a50eed8695804790f9b020100e8117ee1f7bf534b97fd287`; crop SHA-256 was `67d0edfbfc20132aea6f0f4d3e7edddab4a66b7b285a0875763fb5003e40b0c9`. Two real dbusmenu `clicked` events while held produced exactly `reconnect_calls=1`; release restored the healthy icon and menu. |
| Authorization and dashboard | Exact dbusmenu activations produced `authorize_calls=1`, `oauth_page_calls=1`, `dashboard_calls=1`, and exact targets `/oauth-approved?source=tray` and `/`. Unmodified `/usr/bin/xdg-open` selected the disposable desktop handler, which recorded `1<TAB>http://127.0.0.1:7421/oauth-approved?source=tray` and `1<TAB>http://127.0.0.1:7420/`, proving one argument each. |
| Daemon loss and recovery | Stopping only the API listener produced exact offline labels in 5.196 seconds while PID 213 survived. `IconPixmap` was `4690f63c15ccaf0e98ca05b71f565f7fff05819a5a08391e382701fe080d19a8`; crop SHA-256 was `33c9d4be9b1818e5ea375cad73e6a3e40366f8c1fa71d8151db4f32644b24317`. Restart restored the original healthy digest in 4.844 seconds with the same PID. All three 24 by 28 crop hashes were pairwise distinct. |
| Clean Quit | Loaded `RestartUSec=5s` matched the unit's `RestartSec=5s`. Actual dbusmenu Quit from PID 213 remained `ActiveState=inactive`, `SubState=dead`, `Result=success`, and `MainPID=0` after six seconds. |
| Crash restart | SIGKILL of PID 927 produced replacement PID 1033 after 5.375 seconds, no earlier than loaded `RestartUSec`. Actual Quit on the replacement again remained inactive/dead with success and no PID after six seconds. |

The container, image tag, temporary build context, compiled fixture, fixture source, screenshots, runtime logs, and helper files were removed. No Task 7.2 container, image, process, listener, or temporary file remained.

## Final automated gate, 2026-08-20

The complete gate was rerun from the final source and evidence tree on macOS 26.5.2 build 25F84 arm64 with `go version go1.26.6 darwin/arm64`:

```bash
go test -count=1 -race ./...
go vet ./...
go tool govulncheck ./...
plutil -lint dist/dev.mcpd.tray.plist
go run github.com/goreleaser/goreleaser/v2@v2.17.1 release --snapshot --clean --skip=sign
# exact four-archive, nine-member, and checksum assertions from Task 7.1
openspec validate mcpd-tray --strict
git diff --check
```

Every package in the race suite passed. Vet returned no findings. `govulncheck` found zero reachable or imported-package vulnerabilities and three advisories only in required module packages mcpd does not call. Plist lint reported OK. GoReleaser produced snapshot `0.2.1-SNAPSHOT-5d47b0a` for Darwin and Linux on amd64 and arm64; all four archives contained the exact nine required members and all four checksums passed. Strict OpenSpec validation reported `Change 'mcpd-tray' is valid`, and `git diff --check` returned no errors.

## Cumulative adversarial code review

Grok, through the Cursor Agent CLI, independently reviewed branch mode against base `60a5cf3` with repository context and self-collection. The scope included the tray command and tests, native adapter and controller, web projection, dependency pins, startup definitions, CI and release configuration, and user documentation. It also inspected whether the real-session evidence supported the platform claims. No file was skipped.

Iteration 1 verdict: `APPROVED`.

> The tray change set is internally consistent: loopback-only client, shared `recommended_action` classification, non-blocking native callbacks, exit-zero Quit vs non-zero crash, and zero-CGO packaging. Real-session evidence matches the supervisors and the native loop.

The reviewer raised no adversarial findings. Its only residual-risk note was operational and already documented: GNOME requires a compatible StatusNotifier host, and the opt-in startup definitions expect `mcpd` under `~/.local/bin`. Statistics: one iteration, zero findings raised, zero addressed, zero deferred, and zero disagreed.
