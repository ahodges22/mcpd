# Native Tray Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish one narrowly patched systray fork and connect the existing `MenuModel` to a tested native macOS/Linux tray adapter without changing mcpd's zero-CGO, single-executable release contract.

**Architecture:** Keep the upstream fork's default branch aligned with `gogpu/systray`; carry the mcpd patch on `mcpd-main-thread-mutations` and require its immutable commit directly under the fork's module path. The fork gives runtime icon and whole-menu mutations one AppKit actor with explicit pre-run, running, stopping, and stopped states. In mcpd, a small driver boundary converts complete `MenuModel` snapshots to native menu trees and keeps dependency types out of the controller.

**Tech Stack:** Go 1.26.5, `github.com/ahodges22/systray`, AppKit through goffi, StatusNotifierItem through godbus, OpenSpec, Go race detector.

## Scope / Out of scope

**In scope:**

- Create `github.com/ahodges22/systray` and one patch branch aligned with upstream.
- Marshal runtime macOS icon and complete-menu mutations to the AppKit main thread, with real-thread regression probes.
- Pin the verified fork commit in mcpd and update required license notices.
- Convert complete `MenuModel` snapshots into the dependency's native icon and nested-menu representation.
- Prove lifecycle, callback, zero-CGO build, vulnerability, and real-session behavior required by OpenSpec Task 5.1.

**Out of scope:**

- The `mcpd tray` command, browser process launchers, signal handling, and exit-outcome mapping from Task 5.2.
- Linux systemd and macOS LaunchAgent supervision or release packaging from Section 6.
- Windows support.
- Unrelated cleanup or feature work in the systray fork.
- Publishing an upstream pull request; the maintained fork is the approved production source for this task.

## Global Constraints

- Preserve `CGO_ENABLED=0` builds for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.
- Keep the daemon headless and the tray in a separate `mcpd tray` process.
- Carry only the generic main-thread mutation patch and the minimal error-returning API needed to consume it safely; do not fork unrelated upstream behavior.
- Pin an immutable fork commit. Do not depend on a mutable branch at build time.
- Native callbacks only enqueue or invoke already non-blocking controller methods.
- On macOS, the final `mcpd tray` entrypoint must call `runtime.LockOSThread()` as its first statement in `main`, before command dispatch or native construction, and must construct and run the adapter on that startup thread. A late lock inside the driver is not a substitute.
- Replace complete icon/menu snapshots. Never merge partial offline and online state.
- Preserve validated backend names and fixed error copy from `MenuModel`; do not expose raw backend errors.
- Do not mark Task 5.1 complete until dependency tests, mcpd tests, vet, vulnerability scan, four cross-builds, and real-session checks pass.

---

### Task 1: Create the maintained fork branch

**Files:**
- External repository: `github.com/ahodges22/systray`
- Branch: `mcpd-main-thread-mutations`
- Base commit: current verified upstream `github.com/gogpu/systray` default-branch HEAD

**Interfaces:**
- Consumes: upstream `internal.darwinTray` and its existing `drainUpdates:` main-thread target.
- Produces: an immutable fork commit whose `go.mod` declares `module github.com/ahodges22/systray`; only Go self-imports required by that module-path change are rewritten. mcpd imports this path directly and does not use a `replace` directive.

- [ ] **Step 1: Create the GitHub fork without cloning into the mcpd worktree**

Run:

```bash
gh repo fork gogpu/systray --clone=false --remote=false
```

Expected: `github.com/ahodges22/systray` exists and reports `parent.full_name` as `gogpu/systray`.

- [ ] **Step 2: Clone the fork into an isolated temporary directory and add upstream**

Run:

```bash
mcpd_dir=$(pwd)
fork_dir=$(mktemp -d /tmp/mcpd-systray-fork.XXXXXX)
git clone https://github.com/ahodges22/systray.git "$fork_dir"
git -C "$fork_dir" remote add upstream https://github.com/gogpu/systray.git
upstream_branch=$(gh api repos/gogpu/systray --jq .default_branch)
git -C "$fork_dir" fetch upstream "$upstream_branch"
git -C "$fork_dir" switch -c mcpd-main-thread-mutations "upstream/$upstream_branch"
```

Expected: the branch has no diff from upstream and both remotes resolve to the intended repositories.

- [ ] **Step 3: Record the exact upstream base before editing**

Run:

```bash
git -C "$fork_dir" rev-parse "upstream/$upstream_branch"
git -C "$fork_dir" describe --tags --always
git -C "$fork_dir" status --short
```

Expected: one full upstream commit SHA, its nearest released tag, and an empty status. Record both independently; do not describe default-branch HEAD as `v0.2.8` unless the SHA actually equals that tag.

---

### Task 2: Prove and fix AppKit mutation dispatch in the fork

**Files:**
- Create: `internal/platform_darwin_mainthread_test.go`
- Create: `internal/platform_darwin_testmain_test.go`
- Create: `internal/testdata/darwin-unlocked-main/main.go`
- Create: `internal/platform_linux_lifecycle_test.go`
- Inspect and preserve: `internal/init.go` startup-goroutine thread pin
- Modify: `internal/platform_darwin.go`
- Modify: `internal/platform_linux.go` for ready/early-stop signaling, atomic closed/error propagation, concurrent-close teardown, and the testable `linuxBus` seam
- Modify: `internal/tray.go` and `internal/tray_test.go` for template-icon error propagation
- Modify: `go.mod`
- Modify: `systray.go` and `systray_test.go` for construction/setter error plumbing
- Modify: Go self-imports in `menu.go`, tests, and examples required to compile the renamed fork module

**Interfaces:**
- Consumes: `darwinTray.target`, `performSelectorOnMainThread:withObject:waitUntilDone:modes:` with `NSRunLoopCommonModes`, and the existing `drainUpdates:` Objective-C callback.
- Produces: `func (t *darwinTray) runAppKitMutation(func() error) error`, used by runtime `SetIcon`, `SetTemplateIcon`, `SetMenu`, `SetTooltip`, and `Show` calls, a lifecycle barrier shared with `Destroy` and `Run`, a `Ready() <-chan struct{}` signal closed immediately before the platform loop is entered, and backward-compatible public error-returning construction/setter methods.

- [ ] **Step 1: Add subprocess probes that observe the real Objective-C main thread**

Add Darwin-only tests with these cases:

```go
func TestSetTemplateIconMarshalsAppKitMutationToMainThread(t *testing.T)
func TestSetIconMarshalsAppKitMutationToMainThread(t *testing.T)
func TestCreateOffMainThreadReturnsError(t *testing.T)
func TestRunOffMainThreadReturnsError(t *testing.T)
func TestSecondRunWhileRunningReturnsErrorWithoutCleanup(t *testing.T)
func TestSetMenuMarshalsAppKitMutationToMainThread(t *testing.T)
func TestSetMenuAppliesInitialDisabledState(t *testing.T)
func TestSetTooltipMarshalsAppKitMutationToMainThread(t *testing.T)
func TestShowMarshalsAppKitMutationToMainThread(t *testing.T)
func TestPreRunMainThreadIconAndMenuSurviveRunSetup(t *testing.T)
func TestAppKitMutationFromWorkerBeforeRunIsDrainedByRun(t *testing.T)
func TestDestroyBeforeRunMakesRunReturnPromptly(t *testing.T)
func TestWorkerDestroyBeforeRunThenRunCleansUpStatusItem(t *testing.T)
func TestMainThreadDestroyBeforeRunCleansUpStatusItemInline(t *testing.T)
func TestMainThreadDestroyBeforeRunThenRunIsSafeNoOp(t *testing.T)
func TestWorkerDestroyWhileRunningMakesRunReturnPromptly(t *testing.T)
func TestMainThreadDestroyWhileRunningMakesRunReturnPromptly(t *testing.T)
func TestAppKitMutationAfterRunLoopExitReturnsError(t *testing.T)
func TestTooltipAndShowAfterRunLoopExitReturnError(t *testing.T)
func TestMutationAndDestroyInTrackingModeReturnPromptly(t *testing.T)
func TestWorkerMutationDispatchNeverTargetsReleasedHandle(t *testing.T)
func TestAppKitMutationRacingDestroyDoesNotHang(t *testing.T)
func TestRunFailureBeforeLoopCompletesPendingMutations(t *testing.T)
func TestPreRunBatchApplyErrorDoesNotStrandWorkers(t *testing.T)
func TestDestroyDuringPreRunBatchSerializesCleanup(t *testing.T)
func TestDestroyAfterRunLoopExitIsNoOp(t *testing.T)
func TestPanickingStolenBatchReleasesWaiters(t *testing.T)
func TestNewWithErrorReturnsCreateFailure(t *testing.T)
func TestErrorReturningSettersPreservePlatformErrors(t *testing.T)
func TestReadySignalsOnlyWhenPlatformRunStarts(t *testing.T)
func TestPackageInitPinsStartupThreadAcrossCreateAndRun(t *testing.T)
```

`internal/platform_darwin_testmain_test.go` must lock the startup OS thread from Darwin-only test initialization, run `m.Run` on a worker, and keep `TestMain` on the locked startup thread executing jobs received over a test-only channel. Use that dispatcher for construction, pre-run inline work, the outer `Run`, and post-exit work only. Once `[NSApp run]` owns thread 0, deliver main-thread test actions through the production mutation actor: a worker enqueues a test mutation, `drainUpdates:` runs it in `NSRunLoopCommonModes`, and that mutation invokes the nested main-thread `Destroy` or second `Run` being tested. This exercises the real actor ordering without a second test-only Objective-C pump and cannot block the outer `TestMain` dispatcher. Tracking-mode inline cases use the same production path. A plain `runtime.LockOSThread()` inside a `TestXxx` goroutine is forbidden because it does not prove thread 0. Darwin `Create` and `Run` must check `NSThread.isMainThread` before native work and return a deterministic error off-main; tests must prove both checks. The probes must directly observe main-thread execution, prove both main-thread and worker pre-run work is not lost, separately prove worker-destroy-then-Run cleanup and main-thread inline cleanup, prove worker and nested-main-thread Destroy while an idle loop is running both make `Run` return within 300 ms with no user event, prove a nested main-thread second `Run` returns the already-running error without cleanup, prove post-exit destroy is a no-op, prove a stolen batch cannot apply concurrently with cleanup, double-complete, or strand waiters on panic, and use a 300 ms bound for lifecycle and destroy-race calls. Generic systray tests use a private fake platform constructor to prove `NewWithError` returns `Create` failures and the new error-returning setters return exact platform errors.

Run `internal/testdata/darwin-unlocked-main` as a separate process whose `main` does not explicitly call `runtime.LockOSThread`. It must force scheduler yields between construction, initial setters, and `Run`, directly query `NSThread.isMainThread` at each stage, and prove the fork's existing `internal/init.go` lock keeps the startup goroutine on thread 0 through cleanup. This subprocess isolates the package-init guarantee from the test harness's own lock.

- [ ] **Step 2: Run the tests against unmodified upstream and capture red**

Run on macOS:

```bash
CGO_ENABLED=0 go test ./internal -run 'Test(SetIcon|SetTemplateIcon|SetMenu|SetTooltip|Show|TooltipAndShow|CreateOff|RunOff|SecondRun|PreRun|AppKit|Destroy|WorkerDestroy|WorkerMutation|MainThreadDestroy|MutationAndDestroy|RunFailure|Panicking|PackageInit)' -count=1
CGO_ENABLED=0 go test . -run 'Test(NewWithError|ErrorReturningSetters|ReadySignals)' -count=1
```

Expected: icon and menu probes report an off-main mutation, pre-run work is lost, a bounded lifecycle probe times out, or the public error-returning API does not compile.

- [ ] **Step 3: Add one lifecycle-gated AppKit mutation actor**

Add these private types and state without reverting upstream snapshot or multi-tray fixes:

```go
type darwinMutation struct {
	apply func() error
	done  chan error // nil for a pre-run, fire-and-drain mutation; otherwise capacity 1
}

type darwinTray struct {
	// existing fields remain unchanged
	mutationMu      sync.Mutex
	mutationState   darwinMutationState
	pendingMutations []darwinMutation
	stopOnce         sync.Once
	stop             chan struct{}
	cleanupOnce      sync.Once
}
```

Preserve the fork's existing `internal/init.go` call to `runtime.LockOSThread`. Go runs package initialization on the startup goroutine, so this is the mechanical pin that prevents migration between the AppKit checks, construction, initial setters, and `Run`; no fork path may call `runtime.UnlockOSThread`. The future command still locks in the first statement of `main` as an explicit process contract, but correctness does not depend on a late adapter-level lock. The separate unlocked-main subprocess is the regression proof for this retained invariant.

Use explicit `beforeRun`, `running`, `stopping`, and `stopped` states; `beforeRun` is the only initial state, and `stopped` is terminal after `Run` exits. `Create`, called synchronously by `systray.New`, already launches `NSApplication` and creates the `NSStatusItem`, so an allowed main-thread mutation in `beforeRun` is applied to real native objects and must remain present when `Run` repeats its idempotent application setup. `runAppKitMutation` must gate on state under `mutationMu`. A permitted main-thread caller unlocks before executing AppKit work inline, so nested AppKit run loops never hold the Go mutex. A worker call before `Run` appends one copied immutable closure and returns without blocking. After its idempotent native setup succeeds, `Run` must re-read state under the mutex: if a stop is already recorded it must not enter `[NSApp run]`; otherwise it atomically enters `running` and steals the pre-run batch, unlocks, applies that batch on the main thread, closes the one-shot ready signal, then enters `[NSApp run]`. A running worker call creates its own `make(chan error, 1)` and, while still holding `mutationMu`, enqueues, reads the target, and dispatches `drainUpdates:` asynchronously with `waitUntilDone:NO` in `NSRunLoopCommonModes`; it unlocks before selecting between its own result and the tray stop channel. The Linux platform closes the same ready contract immediately before blocking on its existing quit channel; an early stop returns without signaling ready.

Define one helper that completes a mutation result with a non-blocking send to its capacity-1 channel; nil or already-completed channels are no-ops. When entering `stopping`, discard any shared pending entry whose terminal result was already completed rather than attempting a second completion. Ownership transfers when a batch is stolen: shutdown may complete only mutations still in the shared pending slice, while the main-thread batch owner alone completes stolen mutations after applying them. Every stolen-batch apply helper must defer completion of all entries it has not completed yet with a terminal error, so normal failure and panic both release every waiter before returning or continuing the panic. This prevents dual completion and panic leaks.

`Destroy` must be idempotent across all states. It atomically enters `stopping` from `beforeRun` or `running`, closes the tray stop channel through `stopOnce`, rejects new mutations, and ensures every pending result completes. Native cleanup remains single-threaded and is protected by the separate `cleanupOnce`. A main-thread `Destroy` in `beforeRun` drains the undispatched pre-run slice and cleans inline. Any `Destroy` while `running`, including from the main thread, appends the destroy sentinel to the same actor queue; the main thread immediately drains it when already on main, while a worker dispatches it asynchronously in `NSRunLoopCommonModes`. Thus cleanup cannot leapfrog queued mutations. A worker `Destroy` before the loop records the stop request without dispatching work to a loop that does not exist.

Every scheduled `drainUpdates:` callback first performs the registry/state check under `mutationMu`, becomes a no-op after cleanup, and completes any owned waiter with the terminal error before returning. Main-thread cleanup sends `cancelTracking` to any active native menu, atomically captures and clears all Go-side native handles under `mutationMu`, unlocks, then removes/releases the captured native objects. For the final tray it must preserve upstream's idle-safe exit sequence: send `[NSApp stop:]`, then post an `NSEventTypeApplicationDefined` event with `postEvent:atStart:YES` so `[NSApp run]` observes the stop without user input. Scheduled Objective-C selectors retain their receiver until invocation, but no post-cleanup callback may dereference cleared Go state or native handles. From terminal `stopped`, `Destroy` is a strict no-op.

The first statement of Darwin `Run` must check `NSThread.isMainThread`. An off-main call returns the exported main-thread-required error immediately, installs no deferred barrier, touches no native state or cleanup once, and leaves lifecycle at `beforeRun` so a later correct main-thread `Run` can succeed. On main, check state under `mutationMu` before installing cleanup: if already `running`, return an explicit already-running error without cleanup; this prevents a nested or duplicate `Run` from tearing down the live tray. For `beforeRun`, `stopping`, or `stopped`, install one deferred barrier whose full sequence executes on every later exit and panic, including the early-stop path: atomically enter `stopped`, close stop through `stopOnce`, steal and complete shared pending mutations, and invoke the shared `cleanupOnce` helper that captures/clears handles under `mutationMu` and releases them after unlocking. Re-read state before native setup; if state is `stopping` or `stopped`, skip setup and the event loop and return nil through that full barrier. Native setup must finish before `Run` enters `running`.

Document the library-level limitation that a worker-thread `Destroy` in `beforeRun` records a durable stop that is cleaned when the caller subsequently invokes `Run`; callers that may never invoke `Run` must destroy on the main thread. mcpd satisfies this because its only no-Run cleanup paths execute synchronously on the startup thread.

If any pre-run worker mutation fails while the stolen batch is applied, complete its waiter if present, log the error, finish the batch while state permits, and continue into the loop; the already-successful synchronous initial adapter apply guarantees a visible baseline, and the adapter waits for ready before issuing any runtime snapshot. A concurrent explicit stop request prevents remaining pre-run mutations; the deferred main-thread cleanup owns teardown, so AppKit apply and destroy never run concurrently. `Run` returns nil when an explicit stop was recorded before the loop. `applyPendingUpdates` must steal a mutation batch under the mutex, unlock, and only then apply it on the main thread; no AppKit work executes while `mutationMu` is held. This lock-and-state protocol must make enqueue versus shutdown linearizable, prevent a selector from waiting on an exited or never-entered loop, and preserve the existing non-blocking `Destroy` contract.

- [ ] **Step 4: Route required setters through the dispatcher and preserve their errors**

Refactor the Darwin implementations to this shape:

```go
func (t *darwinTray) SetTemplateIcon(png []byte) error {
	owned := append([]byte(nil), png...)
	return t.runAppKitMutation(func() error {
		return t.applyIcon(owned, true)
	})
}

func (t *darwinTray) SetMenu(menu *Menu) error {
	// The caller hands off a fresh menu snapshot and does not mutate it afterward.
	return t.runAppKitMutation(func() error {
		return t.applyMenu(menu)
	})
}
```

Apply the same actor path to `SetIcon`, `SetTooltip` (capture `strings.Clone(text)`), and `Show`. Initial main-thread calls before `Run` must use the already-created status item, survive `Run`'s idempotent setup unchanged, and pass `TestPreRunMainThreadIconAndMenuSurviveRunSetup`. Every one of these setters must reject `stopping` and `stopped` before dereferencing native handles. Retain all current menu-item snapshot synchronization and final-tray shutdown behavior. When `buildNSMenu` creates normal, checkbox, or submenu-container items, apply `setEnabled:` from the immutable `disabled` snapshot before attachment; `TestSetMenuAppliesInitialDisabledState` must observe disabled native items on the first complete menu rather than relying on a later live update.

At the start of Darwin `Create` and again before any defer or native work in `Run`, call the same `NSThread.isMainThread` helper and return one exported AppKit-main-thread-required sentinel error if false. The sentinel detects a caller running on the wrong OS thread; it does not claim to detect whether a goroutine was explicitly locked. `NewWithError` and `NewNativeAdapter` propagate the `Create` error unchanged. When `Create` returns that sentinel, `NewWithError` returns nil plus the error without calling `Destroy`, because no native allocation began; partial-resource cleanup applies only to later main-thread construction failures.

`TestCreateOffMainThreadReturnsError` must assert no destroy/cleanup path ran. `TestRunOffMainThreadReturnsError` must assert no cleanup ran, lifecycle remains `beforeRun`, the status item remains live, and a subsequent dispatcher-driven main-thread `Run` enters the loop and signals ready before clean removal.

Add a private constructor helper that returns both tray and `Create` error. Keep `New() *SystemTray` source-compatible with upstream, and add `NewWithError() (*SystemTray, error)` for mcpd. On main-thread failure after native allocation begins, `NewWithError` must destroy partially-created platform resources and return nil plus the original construction error; the exported off-main sentinel follows the no-cleanup rule above. Add error-returning variants `SetIconErr`, `SetTemplateIconErr`, `SetMenuErr`, `SetTooltipErr`, and `ShowErr`; existing fluent methods delegate to these and intentionally discard errors for compatibility. Add `Ready() <-chan struct{}` through a private optional platform interface implemented by Darwin and Linux. For platforms without that optional interface, including unchanged Windows, the public method returns a shared pre-closed channel and documents that only Darwin/Linux provide loop-readiness semantics. Change the internal template-icon path to return its platform error instead of swallowing it. mcpd must use only the error-returning constructor and setters and must wait for `Ready` before runtime applies.

On Linux, close ready only when `Run` is about to wait on the quit channel and preserve the already-closed quit behavior for `Remove` before `Run`. Use an atomic closed flag and `sync.Once`, not a writer lock that can wait behind a stalled setter. Put dynamic property/signal operations and connection close behind one private `linuxBus` seam so a test bus can block a setter until close. Destruction atomically marks closed, closes the quit channel first so `Run` returns immediately, then closes the private godbus connection. Rely on godbus v5's documented `Conn.Close` contract that blocked operations return errors; closing the private connection also releases its well-known name, so do not make a separate blocking `ReleaseName` call. Keep the connection and property pointers immutable after `Create` so close cannot race pointer clearing. Every setter checks closed before native work, converts property-emission failures caused by concurrent close into returned errors, and treats `dbus.ErrClosed` as the fixed terminal error. Subsequent setters return that terminal error without touching the connection. A second `Remove` is a strict no-op. `TestLinuxDestroyUnblocksSetter` must use the seam to block inside a property/signal operation, call `Destroy`, require quit and setter release within 300 ms, and require later setters to return the terminal error under `-race`.

Linux uses the pure-Go StatusNotifierItem/godbus backend, not GTK or another thread-affine UI toolkit. Runtime setters remain on the adapter worker; existing state mutexes protect model fields and godbus protects connection send/close concurrency internally. The Linux `go test -race` live-replacement and remove-during-setter probes in Step 6 are the acceptance gates for that contract.

- [ ] **Step 5: Run focused, race, and zero-CGO dependency verification**

Run:

```bash
# Run both of these on macOS and on Linux.
CGO_ENABLED=0 go test ./... -count=1
go test -race ./... -count=5

# Run this dispatch-vs-destroy stress case on macOS.
go test -race ./internal -run '^TestWorkerMutationDispatchNeverTargetsReleasedHandle$' -count=100

# Cross-build the supported release matrix plus unchanged Windows compatibility.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
```

Expected: every command exits zero.

- [ ] **Step 6: Prove Linux lifecycle and open-menu replacement behavior before publishing**

Before publishing the fork commit, run a real Linux session-bus probe that calls `New`, `SetIcon`, `SetMenu`, `SetTooltip`, and `Show` before `Run`. Query the exported StatusNotifierItem properties and dbusmenu layout from a second D-Bus connection. Prove `Remove` before `Run` makes the later `Run` return nil within 300 ms without signaling ready, and prove another `Remove` after `Run` returns is a no-op. In separate `go test -race` cases, require ready and repeatedly replace icon and menu snapshots: first verify final exported state before ordinary removal, then call `Remove` while a setter is active and require no race or panic, `Run` to return within 300 ms, and every concurrent or later setter to return the fixed terminal error. Run the visibility cases with a tray host present and with the watcher absent, then start the watcher late and require the already-applied icon and menu to appear without another setter call. With `DBUS_SESSION_BUS_ADDRESS` unset, require `NewWithError` to return a synchronous non-nil construction error without panic or hang.

Add `TestLinuxSetMenuAppliesInitialDisabledState`. Through the second D-Bus connection, require the first complete layout to contain `enabled=false` for disabled normal, checkbox, and submenu-container items; do not require enabled items to emit `enabled=true`, because dbusmenu defaults an omitted property to true. Also send `Event("clicked")` to an enabled zero-command item and a disabled actionable item and require neither to panic nor invoke the sentinel callback. Verify from the exact pinned source that `itemProperties` derives the initial `enabled` property from the immutable disabled snapshot. If it does not, add the symmetric Linux initial-state fix and rerun this probe before continuing.

On both Linux and macOS, hold a native menu open while replacing it with a genuinely changed tree. Require the setter call itself to return within 300 ms and confirm the icon visibly changes while tracking remains active. Then call `Remove` during tracking and require native `Run` to return within 300 ms. Record the AppKit run-loop dispatch mode and whether each platform dismisses the open menu. If a platform dismisses on change, implement and test platform tracking callbacks that accept and coalesce the latest changed menu immediately, return from the setter within the bound, and apply that menu on close; icons remain immediate. These delivery assertions distinguish intentional deferral from a selector that never ran.

Expected: `Create` has already exported the SNI properties before `Run`; the complete pre-run snapshot, including disabled state, is observable in every case and survives late watcher registration; nil-callback clicks are inert; early removal returns promptly; repeated live replacements are race-free and converge on the final snapshot; and genuinely changed open-menu behavior is either safe or mitigated before publication. Record the result in `phase0-tray-feasibility.md`. If state is dropped or a race is detected, stop before publishing and choose the smallest proven serialization or ready/re-apply fix; do not silently broaden the fork patch without evidence.

The published fork SHA must have Step 5 and Step 6 evidence recorded against its final contents. If Step 6 changes any fork Go file, rerun all of Step 5 on macOS and Linux, including both test commands and all five cross-builds, then rerun every Step 6 probe. Any later fork source edit invalidates that evidence and must repeat the same sequence before Step 7.

- [ ] **Step 7: Review the fork diff against upstream**

Run:

```bash
git -C "$fork_dir" diff "upstream/$upstream_branch"
git -C "$fork_dir" show HEAD:LICENSE
if git -C "$fork_dir" cat-file -e HEAD:NOTICE 2>/dev/null; then
  git -C "$fork_dir" show HEAD:NOTICE
fi
```

Require an independent review to confirm the diff contains only the Darwin mutation actor and initial-disabled-state fix, the Linux lifecycle changes, minimal construction/setter error plumbing, tests, fork module-path declaration, and required Go self-import rewrites; preserves `internal/init.go`'s startup-goroutine `runtime.LockOSThread` with no unlock path; does not weaken existing snapshot synchronization; does not change Windows native behavior; and has licensing/notice treatment matching the inspected pinned artifacts. From the exact pinned source, also confirm and record in `phase0-tray-feasibility.md` that `Menu.Add`, `AddSubmenu`, and `AddSeparator` only mutate unattached Go values, `MenuItem.SetDisabled` dispatches only when its updater is non-nil, and the updater is first wired by `SetMenu`. Confirm that Darwin `menuItemClicked:` and Linux `Event` nil-check callbacks before invocation and that each platform replaces its callback/item map for a complete menu rather than retaining a previous callback; confirm Linux `itemProperties` exports `enabled=false` from the initial disabled snapshot. If the menu-construction facts differ, stop and move native menu construction into the AppKit mutation closure. If the click guards, map replacement, or Linux initial-disabled behavior differ, stop and add the smallest platform fix and its direct native test before publication.

- [ ] **Step 8: Commit and publish the immutable fork revision**

Run:

```bash
git -C "$fork_dir" add go.mod systray.go menu.go systray_test.go internal/tray.go internal/tray_test.go internal/platform_darwin.go internal/platform_darwin_mainthread_test.go internal/platform_darwin_testmain_test.go internal/testdata/darwin-unlocked-main/main.go internal/platform_linux.go internal/platform_linux_lifecycle_test.go examples
git -C "$fork_dir" commit -m "fix: make runtime tray lifecycle safe"
git -C "$fork_dir" push -u origin mcpd-main-thread-mutations
fork_commit=$(git -C "$fork_dir" rev-parse HEAD)
go -C "$mcpd_dir" list -m -json github.com/ahodges22/systray@"$fork_commit"
git -C "$fork_dir" status --porcelain
```

Expected: the printed SHA is the only fork revision mcpd will pin, Go resolves that SHA to one pseudo-version under the fork module path, status output is empty so no required module-rename or test change was omitted, and the recorded Step 5 plus Step 6 evidence is for this exact SHA with no later fork source edit.

---

### Task 3: Define the mcpd native adapter lifecycle with tests

**Files:**
- Create: `internal/tray/native.go`
- Create: `internal/tray/native_test.go`

**Interfaces:**
- Consumes: `MenuModel`, `MenuItem`, `MenuCommand`, `BuildOfflineMenu()`, `TrayIcon.Bytes()`, and `<-chan MenuModel` from `Controller.Updates()`.
- Produces: `NativeAdapter.Run(context.Context, <-chan MenuModel) error` and a private driver boundary used by Task 4.

- [ ] **Step 1: Write `TestNativeAdapterLifecycle` against a fake driver**

The test must require this lifecycle:

```go
type nativeDriver interface {
	Apply(MenuModel) error
	Ready() <-chan struct{}
	Run() error
	Remove()
}

type NativeAdapter struct {
	driver nativeDriver
}

func (a *NativeAdapter) Run(ctx context.Context, updates <-chan MenuModel) error
```

The fake's normal `Run` must faithfully model both supported drivers: `Remove` closes a durable stop signal, so `Run` returns immediately even when removal happened first, and a controlled ready channel closes only after the fake loop starts. The test must prove the adapter applies an initial offline model synchronously, coalesces snapshots arriving before ready without calling runtime `Apply`, applies only the newest one after ready, retries the next full snapshot after a runtime apply error, forwards a driver `Run` error unchanged, and calls `Remove` exactly once when the context is cancelled or the update channel closes. Add an initial-apply failure case that logs but still enters native `Run` and successfully applies the next complete snapshot; an already-cancelled context case that performs no apply or native Run; a removal-before-`Run` race case; a driver failure case while apply is blocked; and a context-cancellation case while apply is blocked. Stream snapshots through the coordinator while the fake repeatedly blocks and releases `Apply`, then cancel and require the latest-slot publisher never blocks. Every blocked case must require the coordinator to call `Remove` before any join, release the apply worker, and return within 300 ms. Clean cancellation and update-channel closure return nil. Every case must prove both bounded goroutines have exited.

- [ ] **Step 2: Run the lifecycle test and capture red**

Run:

```bash
go test ./internal/tray -run '^TestNativeAdapterLifecycle$' -count=1
```

Expected: build failure because `NativeAdapter` and `nativeDriver` do not exist.

- [ ] **Step 3: Implement the minimal lifecycle**

`Run` first checks whether the context is already cancelled; if so, remove synchronously and return nil without applying or entering the native loop. Otherwise synchronously apply `BuildOfflineMenu()` on its caller before starting goroutines. Construction errors were already returned by `NewNativeAdapter`; a native initial-apply error is recoverable, so log it and continue into the same coordinator/apply/native-loop lifecycle. Re-check cancellation after the apply and cleanly remove/return nil if it arrived during that call. The next guaranteed complete controller snapshot retries every field after ready.

Otherwise start exactly two bounded goroutines. The coordinator exclusively reads the caller context and external update channel, owns the exactly-once removal path after startup, and publishes recursively copied snapshots into a mutex-protected latest-model slot plus a capacity-1 wake channel. Publishing replaces the slot under the mutex and performs only a non-blocking wake send; it never drains from a channel shared with the consumer. Context cancellation, update-channel closure, or a `runDone` signal closes an internal shutdown channel, calls `driver.Remove()` before any join, stops reading external updates, and only then closes the wake channel as its sole sender. The apply worker selects on shutdown while waiting for ready, then receives wake with the two-value form and exits when `ok` is false. For a live wake it atomically takes and clears the current slot, skips a stale wake with no value, and applies only the newest snapshot. It never calls `Remove`. A runtime apply uses a complete fresh snapshot; on error, log and continue only if shutdown is not closed so a later snapshot retries every field, while errors returned after shutdown are expected noise and are suppressed.

The caller runs `driver.Run()` on the startup thread. When it returns, close `runDone`, wait for the coordinator to execute or confirm removal, then join the apply worker and return the original driver error. The fork's stop channel/barrier on Darwin and concurrent godbus connection close on Linux must release any blocked native call; later Linux applies are rejected by the atomic closed flag. Runtime apply failures are retried by the controller's guaranteed complete snapshot on every five-second poll, even when backend state is unchanged. Clean cancellation and channel closure return nil. Do not add command parsing or browser opening from Task 5.2.

Document that on macOS construction, the initial synchronous apply, and `Run` must all occur on the startup thread locked at the first statement of `main`. The initial native calls are safe because the fork executes calls from `NSThread.isMainThread` inline; the update worker uses the mutation actor. Do not add a late `runtime.LockOSThread()` inside `NativeAdapter.Run`, because it cannot move a goroutine back to the startup thread.

- [ ] **Step 4: Prove lifecycle behavior under the race detector**

Run:

```bash
go test -race ./internal/tray -run '^TestNativeAdapterLifecycle$' -count=100
```

Expected: PASS with no races or leaked-worker timeout.

---

### Task 4: Add the systray driver, dependency pin, and notices

**Files:**
- Create: `internal/tray/native_systray.go`
- Create: `internal/tray/native_systray_test.go`
- Create: `internal/tray/testdata/darwin-session/main.go`
- Modify: `internal/tray/native.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `THIRD-PARTY-NOTICES`
- Modify: `openspec/changes/mcpd-tray/phase0-tray-feasibility.md`
- Modify: `openspec/changes/mcpd-tray/tasks.md`

**Interfaces:**
- Consumes: `nativeDriver.Apply(MenuModel) error`, `github.com/ahodges22/systray.SystemTray`, and recursive `systray.Menu` construction.
- Produces: `NewNativeAdapter(func(MenuCommand, string)) (*NativeAdapter, error)`, with dependency types contained in `native_systray.go`.

- [ ] **Step 1: Write pure conversion tests and capture red**

Define a small local menu sink interface that represents only the operations the model needs: add item, add separator, add submenu, and set disabled. Add table-driven tests using a recording sink for nested ordering, separators, disabled propagation, and actionable callbacks. Explicitly require informational, disabled, unknown-command, and zero-command items to record a nil callback, while each enabled defined command records one. Invoke the actionable callbacks out of order and require the exact value-captured `(MenuCommand, backend)` for that item, including two adjacent backends that would expose loop-variable capture. Define a second narrow native-tray handle interface for the concrete driver and use a fake to prove `Apply` attempts every field, returns errors matching every injected setter failure, and retries `ShowErr` on the next snapshot after a failed show. Add menu-change cases requiring two structurally identical consecutive `MenuModel.Items` trees to call `SetMenuErr` only once, a changed label/command/backend/disabled/separator/child tree to replace the whole menu, and a failed `SetMenuErr` not to update the cached signature.

Add `TestTemplateIconAlphaMasksAreDistinct`, which decodes all three embedded PNGs and compares alpha masks rather than RGB. macOS template images discard color, so healthy, attention, and offline must differ by shape/alpha. If this test fails, use `SetIconErr` rather than `SetTemplateIconErr` on Darwin and record that decision before acceptance.

Run:

```bash
go test ./internal/tray -run '^(TestBuildNativeMenu|TestSystrayDriverApply|TestTemplateIconAlphaMasksAreDistinct)$' -count=1
```

Expected: build failure because the pure conversion function and sink do not exist.

- [ ] **Step 2: Implement recursive complete-menu conversion and the thin systray sink**

Build each model snapshot through the local sink into a fresh `systray.Menu`. Keep recursive traversal dependency-free; the concrete systray sink is only a thin passthrough. Separators call `AddSeparator`; submenu items recurse through `AddSubmenu`; disabled native items call `SetDisabled(true)`; actionable items capture command and backend by value before registering callbacks:

```go
command, backend := item.Command, item.Backend
var onClick func()
if !item.Disabled {
	switch command {
	case CommandReconnect, CommandAuthorize, CommandDashboard, CommandRetry, CommandQuit:
		onClick = func() { activate(command, backend) }
	}
}
native := menu.Add(item.Label, onClick)
native.SetDisabled(item.Disabled)
```

Create that callback only when `item.Command` is a defined actionable command and the item is enabled. Informational, disabled, and zero-command leaf items must be added with no callback, so a zero-valued command can never reach `activate`.

`NewNativeAdapter` must call `systray.NewWithError` on the already-locked startup thread and return its construction error before creating an adapter. The driver must expose the tray's read-only `Ready` channel; use `SetTemplateIconErr(model.Icon.Bytes())` on Darwin only when `TestTemplateIconAlphaMasksAreDistinct` passes, otherwise use `SetIconErr`; use `SetIconErr` on other platforms; set tooltip `mcpd` with `SetTooltipErr`; show once with `ShowErr`; run the native loop on the caller's startup thread; and delegate idempotent removal to the adapter. Before building native menu objects, recursively compare the incoming `MenuModel.Items` with the last successfully applied tree across label, command, backend, disabled, separator, ordering, and children. Structurally identical trees skip `SetMenuErr`; changed trees still build a fresh menu and replace it wholesale. Cache a deep copy only after `SetMenuErr` succeeds, so failure retries next time and caller mutation cannot corrupt change detection. `Apply` attempts every applicable field in the complete snapshot and returns `errors.Join` of failures; it records `shown` only after `ShowErr` succeeds, so the next complete snapshot retries a partial failure. Every native menu handoff is fresh and never mutated afterward. Never call the compatibility API that discards errors.

- [ ] **Step 3: Pin the fork revision directly and record its upstream base**

Import `github.com/ahodges22/systray` directly in `native_systray.go`. Resolve and require the fork SHA; resolve the upstream base separately for evidence only:

```bash
upstream_base=$(git -C "$fork_dir" rev-parse "upstream/$upstream_branch")
fork_commit=$(git -C "$fork_dir" rev-parse HEAD)
upstream_version=$(go list -m -f '{{.Version}}' github.com/gogpu/systray@"$upstream_base")
go get github.com/ahodges22/systray@"$fork_commit"
go mod tidy
go list -m -json github.com/ahodges22/systray
go build ./internal/tray
```

Expected: `go.mod` has one direct `github.com/ahodges22/systray` requirement at the resolved immutable pseudo-version, no `replace` directive, and no `github.com/gogpu/systray` import or requirement. The recorded upstream version and SHA are evidence for the fork base, not a second build-graph dependency.

- [ ] **Step 4: Update third-party and feasibility records**

Read the actual license artifacts from the pinned commit before writing notices:

```bash
git -C "$fork_dir" show "$fork_commit:LICENSE"
if git -C "$fork_dir" cat-file -e "$fork_commit:NOTICE" 2>/dev/null; then
  git -C "$fork_dir" show "$fork_commit:NOTICE"
fi
```

Verify and record the SPDX identifier, copyright, complete license text, and presence or absence of upstream `NOTICE`. The currently inspected upstream base is MIT with no `NOTICE`, but the pinned commit is authoritative. Append that exact systray license and every newly linked transitive license to `THIRD-PARTY-NOTICES`. If the pinned license or notice obligations differ, stop and satisfy their modification/redistribution requirements before publishing binaries. Record the fork commit, pseudo-version, upstream base, checksums, dependency graph, and exact verification commands in `phase0-tray-feasibility.md`.

- [ ] **Step 5: Run the complete automated Task 5.1 gate**

Run:

```bash
go test -count=1 -race ./...
go vet ./...
go vet ./internal/tray/testdata/darwin-session
go tool govulncheck ./...
CGO_ENABLED=0 go build -o /tmp/mcpd-darwin-session ./internal/tray/testdata/darwin-session
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/mcpd-linux-amd64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/mcpd-linux-arm64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o /tmp/mcpd-darwin-amd64 ./cmd/mcpd
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/mcpd-darwin-arm64 ./cmd/mcpd
openspec validate mcpd-tray --strict
git diff --check
```

Expected: every command exits zero. Record any omitted live-desktop coverage rather than inferring it from builds.

- [ ] **Step 6: Repeat the real-session mutation checks**

Use the real `internal/tray/testdata/darwin-session` main package for macOS acceptance; its first statement in `main` must call `runtime.LockOSThread()` before constructing the adapter. A `TestXxx` goroutine is not an acceptable substitute. Verify one `NSStatusItem`, no Dock icon, healthy/attention/offline icon replacement, dynamic backend-set menu replacement, non-blocking repair callback, cancellation while an update races, and idle clean removal with `Run` returning within 300 ms without user input. Click an enabled zero-command test row and a disabled actionable row and require neither to call `activate` or terminate the process; then click an enabled actionable row and require exactly its callback. Hold the menu open across one identical snapshot and require it to remain open. During a genuine healthy-to-attention change, require `Apply` to return within 300 ms, require the icon to visibly change while the menu is still tracking, and record whether the platform dismisses or intentionally defers the changed menu. Cancel the adapter while the menu is open and require `Run` to return within 300 ms. Run equivalent open-menu and click cases on Linux, and query the first dbusmenu layout for `enabled=false` on disabled normal, checkbox, and submenu items. If a genuine changed snapshot dismisses an open menu on either platform, stop before completion and implement the smallest platform mitigation that accepts and retains only the newest changed menu while tracking, returns promptly, and applies it when the menu closes; icons continue updating immediately. On Linux, also verify host present, host absent, and host appearing late; in the absent case the process must stay alive despite recoverable initial apply failures, use the library's existing `NameOwnerChanged` watcher monitor to re-register when the watcher appears, and converge to the latest icon/menu from the controller's complete five-second snapshots without requiring another user action. Repeat the Linux construction probe with `DBUS_SESSION_BUS_ADDRESS` unset and require `NewNativeAdapter` to return a non-nil construction error without applying, entering `Run`, panicking, or hanging. Record exact timings, run-loop mode, results, and any tracking mitigation in `phase0-tray-feasibility.md`.

If these repeated acceptance checks change any mcpd source, rerun Task 4 Step 5 in full and then repeat this step. If they change any fork source, first rerun the fork's Task 2 Steps 5 and 6 against the new fork HEAD, publish and repin that new immutable commit, rerun Task 4 Step 5, and then repeat this step. Do not proceed to Task 4 Step 7 with verification evidence older than either source tree's final edit.

- [ ] **Step 7: Complete independent review and commit Task 5.1**

Require separate spec and standards review axes for the fork pin, license notices matching the pinned `LICENSE`/`NOTICE`, callback boundaries, native lifecycle, and build evidence. After resolving every material finding, mark Task 5.1 complete and run:

```bash
git add internal/tray/native.go internal/tray/native_test.go internal/tray/native_systray.go internal/tray/native_systray_test.go internal/tray/testdata/darwin-session/main.go go.mod go.sum THIRD-PARTY-NOTICES openspec/changes/mcpd-tray/phase0-tray-feasibility.md openspec/changes/mcpd-tray/tasks.md
git commit -m "feat: add native tray adapter"
```
