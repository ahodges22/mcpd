## 1. Tray Dependency Gate

- [x] 1.1 Evaluate the candidate dependency in a temporary module, record license and transitive dependencies in `phase0-tray-feasibility.md`, and prove `CGO_ENABLED=0` builds for Linux and macOS on amd64 and arm64. Proof: the four named `go build` commands recorded with passing output in the artifact. Commit: `docs: record tray dependency build feasibility`.
- [x] 1.2 Run the temporary spike in a real macOS session and record `NSStatusItem`, no-Dock-icon, three-icon replacement, dynamic nested menu, non-blocking callback, and clean-shutdown results. Proof: the `macOS graphical-session acceptance` checklist in `phase0-tray-feasibility.md`. Commit: `docs: verify macos tray feasibility`.
- [x] 1.3 Run the temporary spike in a real Linux session with host present, host absent, and host appearing late; stop the change if no-host behavior is indistinguishable from a generic failure. Proof: the `Linux graphical-session acceptance` checklist in `phase0-tray-feasibility.md`. Commit: `docs: verify linux tray feasibility`.

## 2. Shared Attention Contract

- [x] 2.1 Add the typed optional `recommended_action` projection and make it the single source for the web attention list, labels, paths, and status JSON without changing existing state semantics. Proof: red-before/green-after `TestBackendStatusRecommendedAction` and `TestStatusAPIRecommendedAction`, followed by `go test ./internal/web`. Commit: `feat: expose backend recommended actions`.

## 3. Loopback Tray Client

- [x] 3.1 Implement the no-proxy loopback status client with bounded bodies, a two-second poll budget, minimal response types, and refusal of non-loopback addresses. Proof: `TestClientStatus`, `TestClientStatusErrors`, and `TestClientRejectsNonLoopback` under `go test ./internal/tray`. Commit: `feat: add tray status client`.
- [x] 3.2 Implement bounded reconnect and authorize POSTs, escaped backend paths, single-argument URL opening, and the HTTPS-or-loopback-HTTP authorization allowlist. Proof: `TestClientAction`, `TestClientActionTimeout`, and `TestAuthorizeURL` under `go test ./internal/tray`. Commit: `feat: add tray repair client`.

## 4. Tray Menu and Controller

- [x] 4.1 Add the three embedded monochrome icons and pure menu model for summary, actionable entries, all servers, offline state, dashboard, retry, failure note, and quit. Proof: `TestMenuModel`, `TestMenuModelOffline`, and `TestTrayAssets` under `go test ./internal/tray`. Commit: `feat: add tray status menu`.
- [x] 4.2 Add the controller that refreshes at startup and every five seconds, replaces snapshots atomically, recovers from daemon loss, cancels on shutdown, and keeps native callbacks non-blocking. Proof: `TestControllerPolling`, `TestControllerOfflineRecovery`, and `TestControllerShutdown` under `go test -race -count=5 ./internal/tray`. Commit: `feat: add tray status controller`.
- [x] 4.3 Serialize repair actions, disable or ignore duplicate activation while one is active, refresh after completion, and never replay after an unknown outcome. Proof: `TestControllerSerializesAction`, `TestControllerActionFailure`, and `TestControllerDoesNotReplay` under `go test -race -count=5 ./internal/tray`. Commit: `feat: coordinate tray repair actions`.

## 5. Native Adapter and Command

- [ ] 5.1 Pin the dependency that passed Phase 1, add the minimal native adapter, and update `THIRD-PARTY-NOTICES` while preserving zero-CGO builds. Proof: `TestNativeAdapterLifecycle`, `go vet ./...`, `go tool govulncheck ./...`, and the four release-target build commands. Commit: `feat: add native tray adapter`.
- [ ] 5.2 Add `mcpd tray`, its `--addr` flag, signal and exit-outcome handling, and safe macOS and Linux browser openers without changing credential-helper dispatch. Proof: `TestRunTrayFlags`, `TestRunTrayRejectsNonLoopback`, `TestRunTrayExitOutcome`, and `TestOpenURLArgumentBoundary` under `go test ./cmd/mcpd ./internal/tray`. Commit: `feat: add tray command`.

## 6. Session Startup and Distribution

- [ ] 6.1 Add the opt-in Linux graphical-session unit with clean-quit behavior, delayed failure restart, start limits, and no credential passthrough. Proof: `systemd-analyze --user verify dist/mcpd-tray.service` plus the Linux `clean quit and crash restart` checklist in `phase0-tray-feasibility.md`. Commit: `feat: add linux tray supervision`.
- [ ] 6.2 Add the opt-in Aqua-only macOS LaunchAgent with unsuccessful-exit keepalive and crash throttling, verified against the target host's `launchd.plist(5)`. Proof: `plutil -lint dist/dev.mcpd.tray.plist` plus the macOS `clean quit and crash restart` checklist in `phase0-tray-feasibility.md`. Commit: `feat: add macos tray supervision`.
- [ ] 6.3 Package both startup files, extend snapshot archive assertions, and document manual use, opt-in installation/removal, Quit, `xdg-open`, offline behavior, and the GNOME host requirement. Proof: the CI release-content script and `goreleaser release --snapshot --clean --skip=sign` produce four archives containing both files. Commit: `docs: document and package tray mode`.

## 7. Final Evidence

- [ ] 7.1 Run the full automated gate and record exact commands and results in `verification.md`. Proof: `go test -count=1 -race ./...`, `go vet ./...`, `go tool govulncheck ./...`, `git diff --check`, strict OpenSpec validation, and the GoReleaser snapshot all pass. Commit: `test: record tray verification evidence`.
- [ ] 7.2 Run the complete real-session acceptance matrix on macOS and Linux, including healthy, attention, offline recovery, reconnect, OAuth browser launch, dashboard launch, duplicate action suppression, clean Quit, crash restart, login start, no macOS Dock icon, host absence, and late host appearance; then run adversarial code review and resolve every merge-blocking finding. Proof: the signed-off platform matrix and review result in `verification.md`. Commit: `test: complete tray acceptance review`.
