# Tray Packaging and Documentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put both opt-in tray startup definitions into every release archive, pin that content in CI, and document manual tray use plus safe installation and removal on Linux and macOS.

**Architecture:** Extend the existing GoReleaser archive file list and its inline CI content assertions rather than adding an installer or a second verification script. Add one focused status-icon section to the existing supervision guide, with a single discoverability bullet and corrected guide link in the root README. Prove the packaging change with a red-before and green-after four-archive snapshot using the same pinned GoReleaser version as CI.

**Tech Stack:** GoReleaser v2.17.1, GitHub Actions YAML, tar archives, Markdown, OpenSpec.

**Spec:** `openspec/changes/mcpd-tray/specs/desktop-status/spec.md` and `openspec/changes/mcpd-tray/design.md`

## Scope / Out of scope

**In scope:**

- Add `dist/mcpd-tray.service` and `dist/dev.mcpd.tray.plist` to all four release archives.
- Extend the existing CI archive-content loop to require both tray startup files.
- Document `mcpd tray`, `--addr`, Quit, offline recovery, Linux `xdg-open`, the stock-GNOME host requirement, and opt-in install/start/remove commands for both platforms.
- Clarify that the existing untested caveat at the end of `dist/README.md` refers to the daemon LaunchAgent, not the newly verified tray LaunchAgent.

**Out of scope:**

- Automatic installation, package-manager integration, login-item UI, or service-manager abstraction.
- Changing either startup definition or any Go production behavior.
- Claiming full platform support before the Task 7.2 click-through matrix.
- Adding screenshots, notifications, or desktop-specific installation commands for the GNOME extension.

## Global Constraints

- Preserve one zero-CGO `mcpd` executable in each Linux and macOS amd64 and arm64 archive.
- Keep startup installation opt-in and separate from daemon supervision.
- Reuse the existing GoReleaser archive and CI verification blocks; do not add a new script for two assertions.
- Name the official GNOME extension `AppIndicator and KStatusNotifierItem Support` and link `https://extensions.gnome.org/extension/615/appindicator-support/`.
- Do not say that stock GNOME displays the icon without a compatible StatusNotifierItem host.
- Explain that Quit remains stopped for the current session and provide the explicit command to start it again.
- Explain that the tray reads only loopback status and does not need backend credentials.

---

### Task 1: Capture the missing archive members as a failing release assertion

**Files:**
- Read: `.goreleaser.yaml`
- Read: `.github/workflows/ci.yml`
- Later modify: `.goreleaser.yaml`

**Interfaces:**
- Consumes: the current release configuration and the pinned GoReleaser v2.17.1 module.
- Produces: red evidence that the current four archives omit both tray startup definitions.

- [x] **Step 1: Build the unchanged release snapshot**

Run from the repository root:

```bash
go run github.com/goreleaser/goreleaser/v2@v2.17.1 release --snapshot --clean --skip=sign
```

Expected: four archives under `.release/`, built with the existing `CGO_ENABLED=0` configuration.

- [x] **Step 2: Run the future archive assertions and capture red**

Run:

```bash
set -euo pipefail
test "$(find .release -maxdepth 1 -name 'mcpd_*_*.tar.gz' | wc -l | tr -d ' ')" -eq 4
for archive in .release/mcpd_*_*.tar.gz; do
  archive_contents="$(tar -tzf "$archive")"
  grep -qx 'dist/mcpd-tray.service' <<<"$archive_contents"
  grep -qx 'dist/dev.mcpd.tray.plist' <<<"$archive_contents"
done
```

Expected: FAIL on `dist/mcpd-tray.service`, proving the new assertion detects the missing packaging behavior.

---

### Task 2: Package both startup definitions and pin their presence in CI

**Files:**
- Modify: `.goreleaser.yaml`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the existing archive `files` list and release-snapshot job.
- Produces: four archives that contain both daemon and both tray startup definitions, plus CI assertions that fail if either tray definition disappears.

- [x] **Step 1: Extend the GoReleaser file list**

Add these entries immediately after their platform-matching daemon definitions:

```yaml
      - src: dist/mcpd-tray.service
        dst: dist/mcpd-tray.service
      - src: dist/dev.mcpd.tray.plist
        dst: dist/dev.mcpd.tray.plist
```

Keep `dist/README.md`, the daemon definitions, `README.md`, `LICENSE`, and `THIRD-PARTY-NOTICES` unchanged.

- [x] **Step 2: Extend the existing CI content loop**

Capture each archive listing once with `archive_contents="$(tar -tzf "$archive")"`, convert the existing member assertions to read that captured text, and add these exact assertions beside the daemon startup-file assertions:

```bash
grep -qx 'dist/mcpd-tray.service' <<<"$archive_contents"
grep -qx 'dist/dev.mcpd.tray.plist' <<<"$archive_contents"
```

Do not duplicate the archive-count or checksum logic.

- [x] **Step 3: Rebuild and prove all archive members green**

Run the snapshot command again, then require exactly these members in every archive:

```text
mcpd
README.md
LICENSE
THIRD-PARTY-NOTICES
dist/README.md
dist/mcpd.service
dist/mcpd-tray.service
dist/dev.mcpd.daemon.plist
dist/dev.mcpd.tray.plist
```

Also run `(cd .release && shasum -a 256 -c checksums.txt)`. Expected: four archives, all nine named members in each archive, and valid checksums.

---

### Task 3: Document the optional status icon without overstating support

**Files:**
- Modify: `dist/README.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: the shipped `mcpd tray` command, Linux user unit, macOS LaunchAgent, and verified supervisor behavior.
- Produces: copy-pasteable manual, install, restart-after-Quit, and removal guidance.

- [x] **Step 1: Add a focused optional status-icon section**

Add `## Optional status icon` before `## Verifying either one` in `dist/README.md`. It must contain these facts and commands:

```markdown
Run the icon manually with `mcpd tray`. Use `mcpd tray --addr 127.0.0.1:PORT` only when the daemon uses a non-default loopback port. The tray reads the loopback status API, never declarations or backend credentials, and never starts or stops the daemon.

When the daemon is unavailable, the icon remains running in its offline state, offers Retry and Quit, and recovers within one five-second poll after the daemon returns. `Quit status icon` exits successfully, so either supervisor leaves it stopped for the rest of the current session.
```

For Linux, include:

```sh
mkdir -p ~/.config/systemd/user
cp dist/mcpd-tray.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mcpd-tray.service

# Start again after Quit in the current session.
systemctl --user start mcpd-tray.service

# Remove it.
systemctl --user disable --now mcpd-tray.service
rm ~/.config/systemd/user/mcpd-tray.service
systemctl --user daemon-reload
```

State that dashboard and authorization actions require `xdg-open` on `PATH`, normally supplied by the `xdg-utils` package. State that stock GNOME does not provide the required StatusNotifierItem host and link the official `AppIndicator and KStatusNotifierItem Support` extension; without a compatible host the tray remains alive but no icon is visible.

For macOS, include:

```sh
mkdir -p ~/Library/LaunchAgents
cp dist/dev.mcpd.tray.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.mcpd.tray.plist

# Start again after Quit in the current session.
launchctl kickstart gui/$(id -u)/dev.mcpd.tray

# Remove it.
launchctl bootout gui/$(id -u)/dev.mcpd.tray
rm ~/Library/LaunchAgents/dev.mcpd.tray.plist
```

Explain that the tray LaunchAgent is Aqua-only, uses a non-login shell only to expand `HOME`, and clears the inherited environment before starting the tray.

- [x] **Step 2: Correct the old daemon-agent caveat**

Change the final caveat from `The macOS agent has not been run` to `The macOS daemon agent above has not been run`, preserving the rest of its warning. This prevents it from contradicting the verified tray LaunchAgent section.

- [x] **Step 3: Make the feature discoverable in the root README**

Add one `What it does` bullet:

```markdown
- Optionally shows backend health and one-click repair actions in the macOS menu bar or Linux status area.
```

Change the existing startup-guide sentence to:

```markdown
For login-session daemon supervision and optional status-icon startup on Linux or macOS, see [the systemd and launchd guide](dist/README.md).
```

Do not add a second installation guide to the root README.

---

### Task 4: Final gates, Grok review, task state, and commit

**Files:**
- Modify: `openspec/changes/mcpd-tray/tasks.md`
- Modify: `openspec/changes/mcpd-tray/task-6.3-packaging-docs-plan.md`

**Interfaces:**
- Consumes: the complete Task 6.3 diff and four final release archives.
- Produces: completed OpenSpec Task 6.3 and commit `docs: document and package tray mode`.

- [x] **Step 1: Run the complete gate from the final source tree**

Run:

```bash
go test -count=1 -race ./...
go vet ./...
go tool govulncheck ./...
go run github.com/goreleaser/goreleaser/v2@v2.17.1 release --snapshot --clean --skip=sign
openspec validate mcpd-tray --strict
git diff --check
```

Then run the exact CI archive-count, nine-member, and checksum assertions. Expected: every command exits 0, all four archives contain both tray startup files and the guide, and `govulncheck` reports no reachable vulnerabilities.

- [x] **Step 2: Submit the complete uncommitted diff to Grok adversarial code review**

Review archive completeness, CI parity, opt-in semantics, command correctness, Quit/restart guidance, credential claims, offline recovery, Linux `xdg-open`, GNOME host wording, macOS environment wording, the remaining daemon-agent caveat, and scope separation from Task 7. Resolve every in-scope merge blocker and rerun Step 1 after the final source edit.

- [x] **Step 3: Mark Task 6.3 and commit**

After Grok approval, mark only Task 6.3 complete, check this plan's completed steps, stage the exact Task 6.3 files, and run:

```bash
git commit -m "docs: document and package tray mode"
```

Expected: one intentional Task 6.3 commit and a clean worktree. Remove the ignored `.release` snapshot after verification because it is reproducible build output.
