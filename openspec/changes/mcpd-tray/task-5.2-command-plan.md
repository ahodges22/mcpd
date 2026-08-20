# Tray Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the reviewed native adapter into an `mcpd tray` process with loopback-only configuration, clean signal and Quit outcomes, and shell-free browser launching on macOS and Linux.

**Architecture:** Keep command orchestration in `cmd/mcpd/tray.go` behind narrow controller and adapter interfaces so tests do not construct native UI. Native callbacks only enqueue controller work or cancel the command context. Keep browser execution in `internal/tray`, where the existing authorization allowlist is reused before invoking one platform executable with the complete URL as one argument.

**Tech Stack:** Go 1.26.6, `flag`, `signal.NotifyContext`, `os/exec`, the existing `internal/tray` controller and native adapter, OpenSpec.

**Spec:** `openspec/changes/mcpd-tray/specs/desktop-status/spec.md` and `openspec/changes/mcpd-tray/design.md`

## Scope / Out of scope

**In scope:**

- Add `mcpd tray`, its loopback-only `--addr`, signal handling, Quit handling, native-loop outcome classification, and startup-thread lock.
- Add safe macOS and Linux browser process invocation and non-blocking controller dispatch for dashboard actions.
- Preserve credential-helper dispatch precedence and prove all command boundaries with strict test-first checks.
- Run the complete Task 5.2 tests, zero-CGO builds, real-process smoke checks, evidence update, and adversarial code review.

**Out of scope:**

- Linux systemd and macOS LaunchAgent definitions, restart policy, archive packaging, and end-user documentation from OpenSpec Section 6.
- Changes to daemon supervision, daemon configuration, web APIs, backend repair semantics, or the native menu model already completed in Tasks 2 through 5.1.
- Windows tray command support or alternate browser-opening implementations.
- Final release-wide verification and the complete two-platform interaction matrix from OpenSpec Section 7.

## Global Constraints

- Preserve the native credential-helper dispatch as the first command dispatch inside `run`.
- The first statement in `main` is `runtime.LockOSThread()` so macOS construction, initial native apply, and the native loop remain on the startup thread.
- Accept only loopback `host:port` addresses through `tray.NewClient`; reject invalid or non-loopback addresses before native construction.
- Native callbacks must return after one non-blocking enqueue, atomic gate, or context cancellation. They never poll HTTP, wait for an action, or run a browser command.
- Invoke `/usr/bin/open` on Darwin and `xdg-open` on Linux through `exec.CommandContext`, with the full validated target as one argument and no shell.
- SIGINT, SIGTERM, and the Quit menu item are successful outcomes. Construction failures, native loop errors, and an unexplained nil native-loop return are failures.
- Preserve zero-CGO Linux and Darwin builds on amd64 and arm64.

---

### Task 1: Add the safe browser boundary and controller-owned dashboard action

**Files:**
- Create: `internal/tray/openurl.go`
- Create: `internal/tray/openurl_test.go`
- Modify: `internal/tray/client.go`
- Modify: `internal/tray/controller.go`
- Modify: `internal/tray/controller_action_test.go`

**Interfaces:**
- Consumes: `validateAuthorizeURL(string) error`, `Client.baseURL`, and the controller's existing bounded `actionRequests` worker path.
- Produces: `func OpenURL(context.Context, string) error`, `func (c *Client) DashboardURL() string`, and `func (c *Controller) OpenDashboard()`.

- [x] **Step 1: Write the failing browser argument-boundary test**

Add `TestOpenURLArgumentBoundary` in `internal/tray/openurl_test.go`. Pass a valid HTTPS URL containing spaces encoded in its query plus shell metacharacters such as `;`, `$()`, and `&` to a private `openURLWith` seam. Record the executable and argument slice in a fake runner and require exactly one argument equal to the original URL. Table-test Darwin selecting `/usr/bin/open`, Linux selecting `xdg-open`, an unsupported OS returning an error without invoking the runner, an unsafe non-loopback HTTP URL being rejected, and a cancelled context being returned without invocation.

The production seam has these exact types:

```go
type browserCommandRunner func(context.Context, string, ...string) error

func openURLWith(ctx context.Context, raw, goos string, run browserCommandRunner) error
func OpenURL(ctx context.Context, raw string) error
```

- [x] **Step 2: Run the focused test and capture red**

Run:

```bash
go test ./internal/tray -run '^TestOpenURLArgumentBoundary$' -count=1
```

Expected: build failure because `openURLWith` does not exist.

- [x] **Step 3: Implement the minimal browser opener**

In `openURLWith`, call `validateAuthorizeURL(raw)` before selecting the executable. Return the context error before starting a process. Select `/usr/bin/open` only for `darwin`, `xdg-open` only for `linux`, and return a fixed unsupported-platform error otherwise. Invoke the runner as `run(ctx, executable, raw)` so the target remains exactly one argument. `OpenURL` passes `runtime.GOOS` and a runner implemented only as:

```go
func(ctx context.Context, executable string, args ...string) error {
	return exec.CommandContext(ctx, executable, args...).Run()
}
```

Wrap validation, cancellation, lookup, and execution errors without including shell output or changing the target.

- [x] **Step 4: Write controller dashboard tests before changing the controller**

Extend `controller_action_test.go` with `TestControllerDashboardIsNonBlocking`. Construct a real controller over an `actionTestClient`, give it a browser opener that blocks on a channel, call `OpenDashboard`, and require the method to return within 20 ms while the opener runs on the controller's owned action worker. Cancel the controller, release the opener, and require shutdown to wait for the worker. Require the opened target to equal the client's loopback dashboard URL. Also require repeated calls while the one-slot request queue is full not to block.

Change `actionResult` to carry both `command MenuCommand` and `err error`. Dashboard completion logs a fixed warning on failure and does not set the repair-failure menu note. Reconnect and authorize completion retain the current action gate, fixed failure note, refresh, and no-replay behavior.

- [x] **Step 5: Implement and verify dashboard dispatch**

Add `Client.DashboardURL`, set one `dashboardURL` string in `NewController`, and add `Controller.OpenDashboard`, which performs only a non-blocking send of `actionRequest{command: CommandDashboard}`. Extend `runAction` to call `OpenAuthorizeURL(ctx, c.dashboardURL, c.openURL)` for `CommandDashboard`. Keep the existing `actionWG` ownership so controller shutdown waits for a browser command already in progress.

Run:

```bash
go test -race ./internal/tray -run 'Test(ControllerDashboardIsNonBlocking|ControllerSerializesAction|ControllerActionFailure|ControllerDoesNotReplay|ControllerActionShutdown|OpenURLArgumentBoundary)$' -count=20
```

Expected: PASS with no races.

---

### Task 2: Add a dependency-injected tray command and explicit exit outcomes

**Files:**
- Create: `cmd/mcpd/tray.go`
- Create: `cmd/mcpd/tray_test.go`

**Interfaces:**
- Consumes: `tray.NewClient`, `tray.NewController`, `tray.NewNativeAdapter`, `tray.OpenURL`, `Controller.Updates`, `Controller.Run`, `Controller.Repair`, `Controller.Retry`, `Controller.OpenDashboard`, and `NativeAdapter.Run`.
- Produces: `func runTray([]string, trayCommandDeps) error` and `func defaultTrayCommandDeps() trayCommandDeps`.

- [x] **Step 1: Write `TestRunTrayFlags` and `TestRunTrayRejectsNonLoopback`**

Define private command interfaces:

```go
type trayCommandController interface {
	Updates() <-chan tray.MenuModel
	Run(context.Context)
	Repair(tray.MenuCommand, string)
	Retry()
	OpenDashboard()
}

type trayCommandAdapter interface {
	Run(context.Context, <-chan tray.MenuModel) error
}
```

Define `trayCommandDeps` with constructors for the concrete client, controller, adapter, opener, and signal context. Use fakes to require default `--addr 127.0.0.1:7420`, a custom loopback address, rejection of positional arguments, and flag parse errors. For non-loopback, call the real `tray.NewClient` through the default client constructor and prove the adapter constructor was never invoked.

- [x] **Step 2: Write `TestRunTrayExitOutcome`**

Cover these outcomes with a fake adapter and controller:

- Quit callback cancels the command context, native `Run` returns nil, controller exits, and `runTray` returns nil.
- A pre-cancelled signal context is a clean nil outcome.
- A native `Run` error is returned with `errors.Is` preserved after controller shutdown.
- A nil native `Run` return while the context is still active returns `errTrayLoopExited` rather than silently exiting successfully.
- Client or native construction errors are returned before any controller goroutine starts.
- Callback routing sends reconnect/authorize to `Repair`, retry to `Retry`, dashboard to `OpenDashboard`, and Quit only to cancellation. Every fake callback method returns immediately.

- [x] **Step 3: Run command tests and capture red**

Run:

```bash
go test ./cmd/mcpd -run '^TestRunTray(Flags|RejectsNonLoopback|ExitOutcome)$' -count=1
```

Expected: build failure because `runTray`, `trayCommandDeps`, and `errTrayLoopExited` do not exist.

- [x] **Step 4: Implement `runTray`**

Parse a command-local `flag.FlagSet` with only `--addr`, defaulting to `127.0.0.1:7420`, and reject positional arguments. Construct the client first so unsafe addresses fail before native work. Create a SIGINT/SIGTERM context, derive one cancelable command context, construct the controller and adapter, then start the controller in exactly one owned goroutine.

The native activation closure has this complete switch:

```go
switch command {
case tray.CommandReconnect, tray.CommandAuthorize:
	controller.Repair(command, backend)
case tray.CommandRetry:
	controller.Retry()
case tray.CommandDashboard:
	controller.OpenDashboard()
case tray.CommandQuit:
	cancel()
}
```

Run the adapter synchronously on the caller's startup thread. Record whether the context was already cancelled when native `Run` returned, then cancel and join the controller. Return the native error unchanged through `%w`; return nil for a cancelled context plus nil native error; otherwise return the fixed `errTrayLoopExited` sentinel.

- [x] **Step 5: Run command tests under the race detector**

Run:

```bash
go test -race ./cmd/mcpd -run '^TestRunTray(Flags|RejectsNonLoopback|ExitOutcome)$' -count=50
```

Expected: PASS with no races or goroutine timeouts.

---

### Task 3: Wire startup-thread locking and preserve credential-helper precedence

**Files:**
- Modify: `cmd/mcpd/main.go`
- Modify: `cmd/mcpd/main_test.go`

**Interfaces:**
- Consumes: `runTray(os.Args[2:], defaultTrayCommandDeps())`.
- Produces: the public `mcpd tray [--addr <loopback-host:port>]` process mode.

- [x] **Step 1: Add static startup and dispatch-order tests**

Add `TestMainLocksStartupThreadBeforeDispatch`, which parses `main.go` with `go/parser`, finds `func main`, and requires its first statement to be the expression `runtime.LockOSThread()`. Add `TestCredentialHelperDispatchPrecedesTray`, which parses `func run` and requires the first executable statement to remain the `ServeNativeHelperIfRequested` conditional and the tray command branch to occur later. These tests fail if a future refactor moves AppKit construction off the startup thread or lets command parsing intercept a native helper invocation.

- [x] **Step 2: Run the static tests and capture red**

Run:

```bash
go test ./cmd/mcpd -run '^Test(MainLocksStartupThreadBeforeDispatch|CredentialHelperDispatchPrecedesTray)$' -count=1
```

Expected: `main` does not call `runtime.LockOSThread`, and `run` has no tray branch.

- [x] **Step 3: Wire the command**

Import `runtime` in `main.go` and make `runtime.LockOSThread()` the literal first statement in `main`. Do not call `runtime.UnlockOSThread`. In `run`, leave `ServeNativeHelperIfRequested` first, then add the `tray` branch beside `install`, `update`, and `secret`, before daemon flag parsing:

```go
if len(os.Args) > 1 && os.Args[1] == "tray" {
	return runTray(os.Args[2:], defaultTrayCommandDeps())
}
```

- [x] **Step 4: Verify command integration and zero-CGO builds**

Run:

```bash
go test -race ./cmd/mcpd ./internal/tray -count=10
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/mcpd
```

Expected: PASS. These builds now compile the actual native adapter through the final command, closing the non-blocking evidence caveat from Task 5.1 review.

---

### Task 4: Final evidence, adversarial review, and commit

**Files:**
- Modify: `openspec/changes/mcpd-tray/phase0-tray-feasibility.md`
- Modify: `openspec/changes/mcpd-tray/tasks.md`

**Interfaces:**
- Consumes: all Task 5.2 implementation and test evidence.
- Produces: completed OpenSpec Task 5.2 and commit `feat: add tray command`.

- [x] **Step 1: Run the complete gate from the final source tree**

Run:

```bash
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

Expected: every command exits zero and `govulncheck` reports no reachable vulnerabilities.

- [x] **Step 2: Run a real process smoke test**

Start the existing local daemon on a temporary loopback port, launch the final `mcpd tray --addr <port>` binary in the Aqua session, verify `lsappinfo` reports `UIElement`, then send SIGTERM and require exit status zero with the icon removed. Run the Linux arm64 binary under `dbus-run-session` without a watcher, send SIGTERM, and require exit status zero. Run both with a non-loopback `--addr` and require failure before a tray item or D-Bus name is created.

- [x] **Step 3: Submit the complete diff to Grok adversarial code review**

Review command orchestration, callback non-blocking behavior, signal and exit classification, browser argument boundaries, credential-helper precedence, startup-thread locking, and cross-platform build evidence. Resolve every in-scope merge blocker and rerun Step 1 after the final source edit.

- [x] **Step 4: Record evidence, mark Task 5.2, and commit**

Append exact commands and outcomes to `phase0-tray-feasibility.md`, mark Task 5.2 complete only after review approval, and run:

```bash
git add cmd/mcpd/main.go cmd/mcpd/main_test.go cmd/mcpd/tray.go cmd/mcpd/tray_test.go internal/tray/client.go internal/tray/controller.go internal/tray/controller_action_test.go internal/tray/openurl.go internal/tray/openurl_test.go openspec/changes/mcpd-tray/phase0-tray-feasibility.md openspec/changes/mcpd-tray/tasks.md openspec/changes/mcpd-tray/task-5.2-command-plan.md
git commit -m "feat: add tray command"
```

Expected: one intentional Task 5.2 commit and a clean worktree.
