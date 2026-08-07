## Why

The supervised daemon cannot reliably inherit credentials from an interactive shell or an
agent sandbox. This leaves otherwise valid MCP backends disconnected after login, restart, or
headless startup, and makes each client depend on a different environment-import workaround.

Credentials need one explicit, cross-platform path into mcpd that does not expose secret values
in the declaration file, process arguments, logs, status responses, or browser state. Existing
environment-only installations must keep their current behavior unless the user opts in.

## What Changes

- Add an opt-in secret resolver for backend environment values, HTTP headers, and the embeddings
  API key. A present process environment variable, including a present empty value, continues to
  take precedence.
- Add a native credential-store provider for macOS and Linux. Native operations run
  behind a bounded helper-process boundary so an operating-system credential prompt or backend
  hang cannot stop the daemon indefinitely.
- Add an explicit managed-file provider for headless systems that cannot use a session credential
  store. mcpd never selects it as an automatic fallback.
- Resolve secrets per consumer and isolate failures. One unavailable credential disables only its
  dependent backend or embeddings client while unrelated backends continue to serve.
- Add write-only CLI and loopback-panel operations to set, inspect presence, retry, and remove
  allowlisted secrets. Secret values are never returned by status APIs or rendered in the panel.
- Preserve the current warning and empty-string expansion when a referenced variable is absent
  and the `secrets` block is omitted.

## Capabilities

### New Capabilities

- `secret-resolution`: Opt-in, allowlisted resolution semantics, environment precedence,
  consumer-level startup behavior, retries, and targeted reconnects.
- `secret-storage`: Cross-platform native credential storage, explicit managed-file storage,
  bounded operations, secure persistence, and provider health behavior.
- `secret-management`: CLI and loopback-panel set, presence-status, retry, and remove operations
  that never disclose stored values.

### Modified Capabilities

None. This repository does not yet contain archived base specifications.

## Impact

- Configuration gains an optional `secrets` block. Existing declarations without it retain their
  current behavior.
- Backend construction, HTTP header expansion, and embeddings construction gain resolved consumer
  inputs instead of reading only the daemon environment.
- The daemon gains credential-provider lifecycle state, a helper subprocess mode, a small
  presence-only status cache, background consumer retries, and targeted backend reconnects.
- The CLI and loopback web surface gain secret-management commands and routes. Remote management
  does not gain access to secret values or mutation routes.
- State under the configured mcpd state directory gains provider locks, helper markers, and, only
  for the explicit file provider, a credential document protected by operating-system file
  permissions.
- Native providers integrate with macOS Keychain and Linux Secret Service. Any third-party
  adapter must pass platform-specific adoption tests before use.
- The existing `mcpd-supervision` change remains separate. Grafana LiteLLM upstream host-policy
  work and client-specific `node_repl` behavior remain out of scope.
