# macOS Tray Supervision Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship an opt-in Aqua LaunchAgent that starts `mcpd tray` without login-profile credentials, leaves a clean Quit stopped, restarts unsuccessful exits, and limits crash restart frequency.

**Architecture:** Add one LaunchAgent limited to the Aqua session. A non-login `/bin/sh` expands the user's home directory, then immediately replaces itself with `/usr/bin/env -i` and `mcpd tray`, retaining only the small environment needed by the executable and AppKit. Pin the plist contract in a repository test, validate it with the target Mac's plist and launchd documentation, and exercise exit and throttle behavior with a uniquely labelled disposable job that never replaces the installed mcpd binary.

**Tech Stack:** launchd LaunchAgents, property lists, `/bin/sh`, Go 1.26.6 tests, macOS 26.5.2 arm64, OpenSpec.

**Spec:** `openspec/changes/mcpd-tray/specs/desktop-status/spec.md` and `openspec/changes/mcpd-tray/design.md`

## Scope / Out of scope

**In scope:**

- Add `dist/dev.mcpd.tray.plist` as an opt-in LaunchAgent limited to Aqua sessions.
- Restart only unsuccessful outcomes and impose launchd's explicit ten-second spawn throttle.
- Avoid login shells and clear the inherited environment before starting the tray.
- Prove clean termination, unsuccessful-exit restart, throttling, no-daemon survival, UIElement registration, and synthetic-secret exclusion under the current user's real GUI launchd domain.

**Out of scope:**

- Installing or replacing `~/.local/bin/mcpd`, the existing daemon LaunchAgent, or any current launchd job.
- Packaging either startup file or documenting installation and removal. Those belong to Task 6.3.
- Full visible-menu and click-through acceptance. That belongs to Task 7.2.
- Adding a wrapper script, login shell, log file, credential-name denylist, or daemon dependency.

## Global Constraints

- The plist is distributed but not installed or bootstrapped automatically.
- Use label `dev.mcpd.tray` and `LimitLoadToSessionType=Aqua`.
- Use `KeepAlive` as a dictionary with `SuccessfulExit=false`; do not use unconditional keepalive.
- Use `ThrottleInterval=10` exactly. This limits spawn frequency rather than guaranteeing ten seconds after every crash.
- Do not add `RunAtLoad`; the installed `launchd.plist(5)` states that the `SuccessfulExit` keepalive condition already implies it.
- Do not use a login shell, source a profile, or pass the launchd job's inherited environment to the long-running tray process.
- Run `/bin/sh -c` with one constant command that immediately executes `/usr/bin/env -i`, retaining only `HOME`, `PATH=/usr/local/bin:/usr/bin:/bin`, `TMPDIR`, and `LANG` before invoking `$HOME/.local/bin/mcpd tray`.
- Do not change Go production behavior in this task.

---

### Task 1: Pin the LaunchAgent contract with a failing repository test

**Files:**
- Modify: `cmd/mcpd/tray_service_test.go`
- Later create: `dist/dev.mcpd.tray.plist`

**Interfaces:**
- Consumes: the repository-relative path `../../dist/dev.mcpd.tray.plist` from the `cmd/mcpd` package test directory.
- Produces: `TestMacOSTrayLaunchAgent`, which locks the Aqua scope, exact process boundary, unsuccessful-exit keepalive, and throttle.

- [x] **Step 1: Add the contract test**

Append this test without adding a general plist parser:

```go
func TestMacOSTrayLaunchAgent(t *testing.T) {
	raw, err := os.ReadFile("../../dist/dev.mcpd.tray.plist")
	if err != nil {
		t.Fatalf("read macOS tray LaunchAgent: %v", err)
	}
	plist := string(raw)
	wantFragments := []string{
		"<key>Label</key>\n  <string>dev.mcpd.tray</string>",
		"<key>LimitLoadToSessionType</key>\n  <string>Aqua</string>",
		"<key>SuccessfulExit</key>\n    <false/>",
		"<key>ThrottleInterval</key>\n  <integer>10</integer>",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(plist, fragment) {
			t.Errorf("macOS tray LaunchAgent missing %q", fragment)
		}
	}

	const programArguments = `<key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>exec /usr/bin/env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-en_US.UTF-8}" "$HOME/.local/bin/mcpd" tray</string>
  </array>`
	if !strings.Contains(plist, programArguments) {
		t.Error("macOS tray LaunchAgent does not use the exact minimal environment boundary")
	}
	for _, forbidden := range []string{"<key>RunAtLoad</key>", "<key>EnvironmentVariables</key>", "-lc", "/bin/zsh", ".zprofile", ".profile", "dev.mcpd.daemon"} {
		if strings.Contains(plist, forbidden) {
			t.Errorf("macOS tray LaunchAgent contains forbidden %q", forbidden)
		}
	}
}
```

The existing `os`, `strings`, and `testing` imports are sufficient.

- [x] **Step 2: Run the focused test and capture red**

Run:

```bash
go test ./cmd/mcpd -run '^TestMacOSTrayLaunchAgent$' -count=1
```

Expected: FAIL because `dist/dev.mcpd.tray.plist` does not exist.

---

### Task 2: Add the minimal opt-in Aqua LaunchAgent

**Files:**
- Create: `dist/dev.mcpd.tray.plist`
- Test: `cmd/mcpd/tray_service_test.go`

**Interfaces:**
- Consumes: `$HOME/.local/bin/mcpd tray` and the current user's Aqua bootstrap namespace.
- Produces: a valid LaunchAgent with successful-exit suppression and unsuccessful-exit throttling.

- [x] **Step 1: Add the exact plist**

Create:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.mcpd.tray</string>

  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>

  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>exec /usr/bin/env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-en_US.UTF-8}" "$HOME/.local/bin/mcpd" tray</string>
  </array>

  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>

  <key>ThrottleInterval</key>
  <integer>10</integer>
</dict>
</plist>
```

Add concise comments only for the two non-obvious decisions: the shell is non-login and immediately replaced after expanding `$HOME`, and successful exits include Quit and ordinary termination.

- [x] **Step 2: Run the contract test green**

Run:

```bash
go test ./cmd/mcpd -run '^TestMacOSTrayLaunchAgent$' -count=1
```

Expected: PASS.

- [x] **Step 3: Validate plist syntax and host semantics**

Run on the target Mac:

```bash
plutil -lint dist/dev.mcpd.tray.plist
MANWIDTH=120 man 5 launchd.plist | col -b | sed -n '86,88p;148,161p;246,250p'
```

Expected: plist lint passes, and the installed documentation confirms Aqua session limiting, inverse restart when `SuccessfulExit` is false, implied initial launch, and the ten-second spawn interval.

---

### Task 3: Exercise the real GUI supervisor and environment boundary

**Files:**
- Modify: `openspec/changes/mcpd-tray/phase0-tray-feasibility.md`

**Interfaces:**
- Consumes: the final arm64 binary, a temporary copy of the final plist with only its label and binary path changed, and the current user's `gui/<uid>` launchd domain.
- Produces: macOS supervisor acceptance evidence without replacing the installed mcpd binary or existing launchd resources.

- [x] **Step 1: Prepare a uniquely labelled disposable LaunchAgent**

Create a directory with `mktemp -d /tmp/mcpd-task62.XXXXXX`, build the final binary inside it, copy the final plist there, verify that `127.0.0.1:47429` has no listener, then use `plutil -replace` to change only these probe values:

```text
Label = dev.mcpd.tray.task62-probe
ProgramArguments.2 = exec /usr/bin/env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-en_US.UTF-8}" "<validated temporary directory>/mcpd" tray --addr 127.0.0.1:47429
```

Set `MCPD_TASK62_PROBE_SECRET=must-not-pass` with `launchctl setenv` before bootstrap, not with a shell export, so the synthetic value enters the same launchd environment that the job would otherwise inherit. Bootstrap the temporary plist into `gui/$(id -u)`, and arrange an exit cleanup that boots out only `gui/$(id -u)/dev.mcpd.tray.task62-probe`, unsets only that synthetic variable, and removes only the validated `/tmp/mcpd-task62.*` directory. Do not modify `~/.local/bin/mcpd`, `~/Library/LaunchAgents`, or `dev.mcpd.daemon`.

- [x] **Step 2: Prove real Aqua startup, no-daemon survival, and credential exclusion**

Poll `launchctl print gui/$(id -u)/dev.mcpd.tray.task62-probe` until it reports a non-zero PID. Require the process to remain alive for more than two five-second tray polls with no daemon dependency. Require `lsappinfo` to classify that PID as `UIElement`. Check `ps eww -p <pid>` only through quiet predicates, without printing its environment, and require `HOME` to be present and `MCPD_TASK62_PROBE_SECRET` to be absent.

- [x] **Step 3: Prove successful termination remains stopped**

First rerun `go test ./cmd/mcpd -run '^TestRunTrayExitOutcome$' -count=1` to prove the Quit callback still reaches successful command-context cancellation. Send SIGTERM directly to the reported PID, wait longer than `ThrottleInterval`, and require the loaded job to have no PID, report a successful last exit, and remain stopped. This composes the same exit-zero path with the real supervisor without using `launchctl bootout`, which suppresses restart independently of exit status.

- [x] **Step 4: Prove unsuccessful exit restarts under the throttle**

Start the still-loaded job with `launchctl kickstart`, record a monotonic timestamp before the request, obtain its PID promptly, and send SIGKILL before it has been alive for one second. Poll until launchd reports a different non-zero PID. Require the replacement spawn to occur no sooner than nine seconds after the recorded start request, allowing one second of observation tolerance around the documented ten-second spawn floor. For the replacement, record a new monotonic timestamp as soon as its PID appears, crash it within one second, and measure the next replacement against that new timestamp. Require the second spawn-to-spawn interval to be at least nine seconds too, proving the throttle remains effective rather than observing a one-time startup delay.

- [x] **Step 5: Record exact evidence and clean up**

Append a `macOS tray supervisor evidence, 2026-08-19` subsection to `phase0-tray-feasibility.md`. Check only the macOS clean-Quit and unsuccessful-exit supervisor items proven here. Record the OS and launchd versions, lint result, documentation excerpts by line range, job label, UIElement result, synthetic-secret predicates, PIDs and timing intervals, successful last-exit state, and cleanup outcome. Boot out the unique probe, unset the synthetic variable, remove the validated temporary directory, and confirm the label is absent.

---

### Task 4: Final gates, Grok review, task state, and commit

**Files:**
- Modify: `openspec/changes/mcpd-tray/tasks.md`
- Modify: `openspec/changes/mcpd-tray/task-6.2-macos-supervision-plan.md`

**Interfaces:**
- Consumes: the complete Task 6.2 diff and all recorded execution evidence.
- Produces: completed OpenSpec Task 6.2 and commit `feat: add macos tray supervision`.

- [x] **Step 1: Run the complete gate from the final source tree**

Run:

```bash
go test -count=1 -race ./...
go vet ./...
go tool govulncheck ./...
plutil -lint dist/dev.mcpd.tray.plist
openspec validate mcpd-tray --strict
git diff --check
```

Expected: every command exits 0 and `govulncheck` reports no reachable vulnerabilities.

- [x] **Step 2: Submit the complete uncommitted diff to Grok adversarial code review**

Review Aqua-only lifecycle, exact `SuccessfulExit=false` behavior, implied initial launch, throttle interpretation, the non-login shell and `env -i` boundary, AppKit environment sufficiency, real-job evidence, synthetic-secret exclusion, cleanup, and scope separation from Tasks 6.3 and 7.2. Resolve every in-scope merge blocker and rerun Step 1 after the final source edit.

- [x] **Step 3: Mark Task 6.2 and commit**

After Grok approval, mark only Task 6.2 complete, check this plan's completed steps, stage the exact Task 6.2 files, and run:

```bash
git commit -m "feat: add macos tray supervision"
```

Expected: one intentional Task 6.2 commit and a clean worktree.
