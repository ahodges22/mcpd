# Linux Tray Supervision Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship an opt-in systemd user unit that runs `mcpd tray` only with a graphical session, leaves a clean Quit stopped, restarts failures after five seconds, rate-limits crash loops, and prevents backend credential variables from reaching the tray process.

**Architecture:** Add one user unit whose lifecycle is bound to `graphical-session.target`. Start the tray through `/usr/bin/env -i` with a small desktop-only environment allowlist because systemd user managers otherwise pass their complete imported environment to every child. Pin the unit's required semantics in a repository test, validate its syntax with `systemd-analyze`, and exercise exit/restart behavior in a disposable real systemd manager.

**Tech Stack:** systemd user units, Go 1.26.6 tests, Docker/Ubuntu 24.04 for disposable systemd 255 runtime verification, OpenSpec.

**Spec:** `openspec/changes/mcpd-tray/specs/desktop-status/spec.md` and `openspec/changes/mcpd-tray/design.md`

## Scope / Out of scope

**In scope:**

- Add `dist/mcpd-tray.service` as an opt-in user unit wanted by and stopped with `graphical-session.target`.
- Restart only unsuccessful outcomes, wait five seconds before restart, and stop retrying after three starts within sixty seconds.
- Clear the user manager's inherited environment and pass only the desktop/session values needed by StatusNotifierItem and `xdg-open`.
- Prove clean termination, delayed crash restart, start limiting, no-host survival, and synthetic-secret exclusion under a real user manager.

**Out of scope:**

- Packaging the unit in GoReleaser archives or documenting installation, removal, GNOME host requirements, and manual tray use. Those belong to Task 6.3.
- The macOS LaunchAgent, which belongs to Task 6.2.
- Starting, stopping, or changing the existing daemon service.
- Adding a wrapper script, environment file, shell invocation, credential-name denylist, or daemon configuration dependency.

## Global Constraints

- The unit is disabled by default and gains an install symlink only when the user explicitly enables it.
- `PartOf=graphical-session.target` stops the tray with the graphical session; `WantedBy=graphical-session.target` is the only install target.
- Do not add `After=graphical-session.target`: targets order themselves after their wanted units by default, so the reverse ordering can create a cycle.
- `Restart=on-failure` preserves process exit 0, SIGINT, and SIGTERM as clean non-restarting outcomes.
- Use `RestartSec=5s`, `StartLimitIntervalSec=60s`, and `StartLimitBurst=3` exactly.
- Do not use `PassEnvironment`, `EnvironmentFile`, a login shell, or a shell command string.
- Start through `/usr/bin/env -i` and allow only `HOME`, `PATH`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`, `DISPLAY`, `WAYLAND_DISPLAY`, `XAUTHORITY`, `XDG_CURRENT_DESKTOP`, `XDG_SESSION_TYPE`, and `LANG` before invoking `%h/.local/bin/mcpd tray`.
- Do not change Go production behavior in this task.

---

### Task 1: Pin the unit contract with a failing repository test

**Files:**
- Create: `cmd/mcpd/tray_service_test.go`
- Later create: `dist/mcpd-tray.service`

**Interfaces:**
- Consumes: the repository-relative path `../../dist/mcpd-tray.service` from the `cmd/mcpd` package test directory.
- Produces: `TestLinuxTrayService`, which locks the session target, restart policy, rate limits, and exact environment allowlist.

- [x] **Step 1: Add the contract test**

Create a table-driven test that reads the unit and requires these exact logical lines:

```go
func TestLinuxTrayService(t *testing.T) {
	raw, err := os.ReadFile("../../dist/mcpd-tray.service")
	if err != nil {
		t.Fatalf("read Linux tray service: %v", err)
	}
	unit := string(raw)
	wantLines := []string{
		"PartOf=graphical-session.target",
		"StartLimitIntervalSec=60s",
		"StartLimitBurst=3",
		"Type=exec",
		"Restart=on-failure",
		"RestartSec=5s",
		"WantedBy=graphical-session.target",
	}
	for _, line := range wantLines {
		if !hasUnitLine(unit, line) {
			t.Errorf("Linux tray service missing %q", line)
		}
	}

	const execStart = "ExecStart=/usr/bin/env -i HOME=%h PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin XDG_RUNTIME_DIR=%t DBUS_SESSION_BUS_ADDRESS=unix:path=%t/bus DISPLAY=${DISPLAY} WAYLAND_DISPLAY=${WAYLAND_DISPLAY} XAUTHORITY=${XAUTHORITY} XDG_CURRENT_DESKTOP=${XDG_CURRENT_DESKTOP} XDG_SESSION_TYPE=${XDG_SESSION_TYPE} LANG=${LANG} %h/.local/bin/mcpd tray"
	if !hasUnitLine(unit, execStart) {
		t.Errorf("Linux tray service does not use the exact desktop-only environment boundary")
	}
	for _, forbidden := range []string{"PassEnvironment=", "EnvironmentFile=", "/bin/sh", "/bin/bash", "/bin/zsh"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("Linux tray service contains forbidden %q", forbidden)
		}
	}
}

func hasUnitLine(unit, want string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
```

Import only `os`, `strings`, and `testing`. Do not build a general systemd parser.

- [x] **Step 2: Run the focused test and capture red**

Run:

```bash
go test ./cmd/mcpd -run '^TestLinuxTrayService$' -count=1
```

Expected: FAIL because `dist/mcpd-tray.service` does not exist.

---

### Task 2: Add the minimal opt-in user unit

**Files:**
- Create: `dist/mcpd-tray.service`
- Test: `cmd/mcpd/tray_service_test.go`

**Interfaces:**
- Consumes: `%h/.local/bin/mcpd tray`, the standard user bus at `unix:path=%t/bus`, and graphical-session variables already imported into the user manager.
- Produces: an installable `mcpd-tray.service` with a clean-success/nonzero-failure supervisor contract.

- [x] **Step 1: Add the exact unit**

Create:

```ini
[Unit]
Description=mcpd status icon
Documentation=https://github.com/ahodges22/mcpd
PartOf=graphical-session.target
StartLimitIntervalSec=60s
StartLimitBurst=3

[Service]
Type=exec
ExecStart=/usr/bin/env -i HOME=%h PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin XDG_RUNTIME_DIR=%t DBUS_SESSION_BUS_ADDRESS=unix:path=%t/bus DISPLAY=${DISPLAY} WAYLAND_DISPLAY=${WAYLAND_DISPLAY} XAUTHORITY=${XAUTHORITY} XDG_CURRENT_DESKTOP=${XDG_CURRENT_DESKTOP} XDG_SESSION_TYPE=${XDG_SESSION_TYPE} LANG=${LANG} %h/.local/bin/mcpd tray
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=graphical-session.target
```

Keep comments limited to the two non-obvious decisions: `env -i` exists because a user manager otherwise passes its full environment, and `Restart=on-failure` keeps Quit stopped. Do not add unrelated sandboxing directives whose desktop compatibility is not verified.

- [x] **Step 2: Run the contract test green**

Run:

```bash
go test ./cmd/mcpd -run '^TestLinuxTrayService$' -count=1
```

Expected: PASS.

- [x] **Step 3: Validate with systemd's parser**

Run on Linux systemd 255:

```bash
systemd-analyze --user verify dist/mcpd-tray.service
```

Expected: exit 0 with no errors or warnings.

---

### Task 3: Exercise the real supervisor and environment boundary

**Files:**
- Modify: `openspec/changes/mcpd-tray/phase0-tray-feasibility.md`

**Interfaces:**
- Consumes: the final Linux arm64 binary, the unit from Task 2, and an ephemeral Ubuntu 24.04 arm64 container booted with systemd.
- Produces: Linux supervisor acceptance evidence for clean termination, delayed restart, rate limiting, and credential exclusion.

- [x] **Step 1: Prepare a disposable user manager**

Build the final binary and create an ephemeral Ubuntu 24.04 arm64 systemd image. In its booted disposable container, create an unprivileged `probe` user, install the binary at `/home/probe/.local/bin/mcpd`, install the unit at `/home/probe/.config/systemd/user/mcpd-tray.service`, enable lingering only inside the disposable container, and start `user@<uid>.service`. Set these synthetic manager variables:

```bash
systemctl --user set-environment DISPLAY=:91 XDG_SESSION_TYPE=x11 MCPD_PROBE_SECRET=must-not-pass
systemctl --user enable mcpd-tray.service
systemctl --user start mcpd-tray.service
```

Start the service directly because `graphical-session.target` is a passive target with `RefuseManualStart=yes`; actual session-target activation remains part of Task 7. Do not connect the container to production services or mount user config, state, home, credentials, or sockets. The tray may remain offline and the StatusNotifier watcher may be absent.

- [x] **Step 2: Prove no-host survival and credential exclusion**

Read `MainPID` and require the process to remain alive for two poll cycles. Inspect `/proc/<MainPID>/environ` without printing it and require:

```bash
tr '\0' '\n' < "/proc/$main_pid/environ" | grep -qx 'DISPLAY=:91'
tr '\0' '\n' < "/proc/$main_pid/environ" | grep -q '^MCPD_PROBE_SECRET=' && exit 1
```

Expected: the allowlisted display value is present, the synthetic secret is absent, and the journal contains the distinct missing-watcher warning rather than a process exit.

- [x] **Step 3: Prove clean exit does not restart**

Send `SIGTERM` directly to `MainPID`, wait longer than `RestartSec`, and require:

```bash
systemctl --user show mcpd-tray.service -p ActiveState -p SubState -p NRestarts
```

Expected: `ActiveState=inactive`, `SubState=dead`, and `NRestarts=0`. This exercises the same successful command-context cancellation used by the Quit menu callback without treating `systemctl stop` as evidence, since an explicit manager stop suppresses restart independently of exit status.

- [x] **Step 4: Prove delayed failure restart and start limiting**

Load the inactive unit if necessary, then run `systemctl --user reset-failed mcpd-tray.service` to clear the earlier clean-start count. Record `MainPID` and a monotonic timestamp, send `SIGKILL` directly to the process, and poll until a new non-zero `MainPID` appears with `ActiveState=active`. Treat `MainPID=0` as the normal auto-restart gap, not as a new process. Require at least five seconds before the replacement PID becomes active. Repeat crashes within sixty seconds until the fourth start is refused, then require `ActiveState=failed` plus either a start-limit result or the version-equivalent `Start request repeated too quickly` manager event.

Reset the failed unit and remove the disposable container and image after capturing commands, versions, timestamps, PIDs, unit properties, and the relevant journal lines.

- [x] **Step 5: Record exact evidence**

Append a `Linux tray supervisor evidence, 2026-08-19` subsection to `phase0-tray-feasibility.md`. Check only the Linux clean-Quit, failure-restart, and rate-limit supervisor items proven here. Record the exact systemd version, parser command, synthetic environment assertions, restart delay, start-limit result, and cleanup outcome.

---

### Task 4: Final gates, Grok review, task state, and commit

**Files:**
- Modify: `openspec/changes/mcpd-tray/tasks.md`
- Modify: `openspec/changes/mcpd-tray/task-6.1-linux-supervision-plan.md`

**Interfaces:**
- Consumes: the complete Task 6.1 diff and all recorded execution evidence.
- Produces: completed OpenSpec Task 6.1 and commit `feat: add linux tray supervision`.

- [x] **Step 1: Run the complete gate from the final source tree**

Run:

```bash
go test -count=1 -race ./...
go vet ./...
go tool govulncheck ./...
systemd-analyze --user verify dist/mcpd-tray.service
openspec validate mcpd-tray --strict
git diff --check
```

Expected: every command exits 0 and `govulncheck` reports no reachable vulnerabilities.

- [x] **Step 2: Submit the complete uncommitted diff to Grok adversarial code review**

Review graphical-session lifecycle, clean-exit classification, restart delay, start limiting, systemd directive placement, `env -i` correctness, desktop-variable sufficiency, synthetic-secret evidence, and scope separation from Tasks 6.2 and 6.3. Resolve every in-scope merge blocker and rerun Step 1 after the final source edit.

- [x] **Step 3: Mark Task 6.1 and commit**

After Grok approval, mark only Task 6.1 complete, check this plan's completed steps, stage the exact Task 6.1 files, and run:

```bash
git commit -m "feat: add linux tray supervision"
```

Expected: one intentional Task 6.1 commit and a clean worktree.
