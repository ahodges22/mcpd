# Verification evidence

Date: 2026-08-08

Host: macOS 26.5 arm64, Go 1.26.5

## Formatting and static checks

- `gofmt -l .`: passed with no output.
- `go vet ./...`: passed.
- `openspec validate native-credential-store --strict`: passed.

## Tests

- `go test -count=1 ./internal/config ./internal/secretstore ./internal/backend ./internal/searchindex ./internal/web ./cmd/mcpd`: passed, 351 tests in 6 packages.
- `go test -race -count=5 ./internal/secretstore ./internal/backend ./internal/web ./cmd/mcpd`: passed, 1,510 tests in 4 packages.
- `go test ./...`: passed, 548 tests in 19 packages.
- `MCPD_TEST_DARWIN_NATIVE=1 go test ./internal/secretstore -run '^TestDarwinNativeRoundTrip$' -count=1 -v`: passed against the current user's login Keychain. The corpus included printable ASCII, Unicode, a hex-like value, and the 2,048-byte maximum value.
- The Linux native integration test was not run because this host has no Linux Secret Service session. The Linux package tests and cross-builds passed.

## Vulnerability scan

The direct `go tool govulncheck ./...` command could not fetch `https://vuln.go.dev` because outbound Go HTTPS is blocked in the execution sandbox. A direct Go HTTP probe reproduced the same timeout, while curl reached the same hosts.

The scan was repeated against a local checkout of the official `golang/vulndb` database at commit `4a2cb55ee69f6a16c07b5b551676eca8f2065019`, committed on 2026-07-28:

```text
go tool govulncheck -db=file:///.../golang-vulndb/data/osv ./...
No vulnerabilities found.
```

## No-cgo build matrix

Each target was built with a fresh `GOCACHE`, `CGO_ENABLED=0`, and `go build ./cmd/mcpd`:

- `darwin/amd64`: passed.
- `darwin/arm64`: passed.
- `linux/amd64`: passed.
- `linux/arm64`: passed.

## macOS LaunchAgent and Context7 smoke

The installed development binary, the current user LaunchAgent, and the live configuration were used.

- The configuration selects the native provider.
- The LaunchAgent command removes `CONTEXT7_API_KEY` before it starts mcpd.
- `mcpd secret status` reports `CONTEXT7_API_KEY` as `provider-present` for `backend/context7`.
- A Context7 MCP initialize request authenticated with the value read from the `io.mcpd.secrets` Keychain item returned HTTP 200 in 0.30 seconds. The value was passed by pipes and was not printed or placed in process arguments.
- The LaunchAgent's mcpd process could not advertise Context7 tools because outbound Go HTTPS is blocked in this execution sandbox. The same process timed out against several unrelated remote MCP servers. A curl request to Context7 from the same session succeeded. The final tool-advertisement check must be repeated after the LaunchAgent is restarted from an unrestricted user Terminal session.

No secret value was written to this evidence file or to the verification command output.
