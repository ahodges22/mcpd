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
