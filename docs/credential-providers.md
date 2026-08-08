# Credential providers

mcpd can resolve selected `${NAME}` references from a credential provider when the daemon environment does not contain the name. Credential providers are optional. If the `secrets` block is absent, mcpd keeps the environment-only behavior.

Native storage is the recommended provider for an interactive user session. File storage is an explicit alternative for a headless identity that has no usable native credential session. mcpd does not copy environment values into either provider, unlock a provider, or fall back from one provider to another.

## Configure a provider

The native provider uses the macOS Keychain or Linux Secret Service:

```json
{
  "backends": {
    "example": {
      "http_url": "https://mcp.example.com/mcp",
      "headers": {
        "Authorization": "Bearer ${EXAMPLE_TOKEN}"
      }
    }
  },
  "secrets": {
    "provider": "native"
  }
}
```

The file provider stores a restricted snapshot in the mcpd state directory:

```json
{
  "backends": {
    "example": {
      "command": "/usr/local/bin/example-mcp",
      "env": {
        "EXAMPLE_TOKEN": "${EXAMPLE_TOKEN}"
      }
    }
  },
  "secrets": {
    "provider": "file"
  }
}
```

Restart mcpd after you add or change the `secrets` block. The provider can supply exact references in these fields:

- Backend `env` values.
- Backend HTTP header values.
- The variable named by `embeddings.api_key_env`.

Other fields stay literal or environment-only. This includes commands, arguments, URLs, `env_passthrough`, remote-listener configuration, and the `secrets` block itself.

## Resolution order

For each allowlisted name, mcpd uses this order:

1. Use the daemon environment when the name is present. A value that is present but empty still wins.
2. If the name is absent, query the configured provider.
3. If the provider reports a clean miss, preserve the existing missing-variable behavior.
4. If the provider is locked, unavailable, denied, corrupt, timed out, or otherwise inaccessible, mark only the dependent consumers unavailable or pending.

The status panel and `mcpd secret status` show the name, its consumers, the effective source, and a typed provider condition. They never return a stored value. Status is derived from configuration references and does not enumerate unrelated native credentials.

After a stored value changes, mcpd reconnects only consumers that reference that name. An environment value continues to shadow a new stored value until you remove it from the daemon launch environment and restart mcpd.

## Set and manage names

Use a hidden terminal prompt:

```sh
mcpd secret set EXAMPLE_TOKEN
mcpd secret status
mcpd secret retry
mcpd secret remove EXAMPLE_TOKEN
```

When standard input is not a terminal, `secret set` reads through EOF. It removes at most one final LF or one final CRLF. It does not trim or normalize other bytes. Do not put a value in an argument. Arguments are visible in process metadata and shell history.

The local panel at `http://127.0.0.1:7420` provides the same write-only operations. The CLI uses the running daemon first. If the daemon is unavailable, the CLI can access the configured provider directly after it validates the state owner and permissions. A direct native operation still needs the intended user's credential session.

Stored values must meet this portable contract:

- From 1 through 2048 bytes.
- Valid UTF-8.
- Printable characters only. NUL, C0 controls, DEL, newlines, and other non-printing characters are rejected.
- Spaces, printable Unicode, quotes, backslashes, dollar signs, and backticks are preserved byte-for-byte.

## Native sessions

Native entries use the service namespace `io.mcpd.secrets`. The configured reference name is the credential account or key.

### macOS Keychain

mcpd uses the current user's default macOS Keychain through a bounded helper. The daemon and an offline CLI must run as the intended user in that user's unlocked login session. mcpd does not unlock the Keychain.

macOS has no reliable non-prompting lock probe for this no-cgo implementation. If an operation reaches `interaction_required`, mcpd stops automatic value-bearing Keychain attempts. Unlock or authorize the Keychain, then run `mcpd secret retry` or select **Retry provider** in the local panel. This rule prevents recurring credential prompts.

### Linux Secret Service

mcpd requires the intended user's session D-Bus, an owner for `org.freedesktop.secrets`, a default collection, and an unlocked collection. A user service started outside that session can have the correct UID but no usable Secret Service.

Do not use `sudo -u` as a substitute for the login session on either platform. It changes the process identity but does not recreate macOS Keychain authorization or the Linux session D-Bus context.

### Headless identities

A headless service account often has no native credential session. Select the file provider explicitly for that identity. mcpd never makes this choice automatically. File mode works without a desktop session, but any process running as the same UID can read the managed file. Its permissions protect against other ordinary users, not against peer processes under the daemon account.

## File provider and permissions

The default state directory is `${XDG_STATE_HOME}/mcpd` when `XDG_STATE_HOME` is set, otherwise `~/.local/state/mcpd`. File mode stores `secrets.json` with mode 0600 and uses a never-replaced `secrets.lock` sidecar. The state directory has mode 0700. Writes use a restricted temporary file, durable atomic replacement, and a directory sync.

mcpd refuses a state directory or managed artifact that belongs to another UID, grants group or other access, or has an unsafe writable parent. Stop mcpd before ownership repair. Confirm that the target UID is the account that will run the daemon, then repair only the selected state tree:

```sh
state="${XDG_STATE_HOME:-$HOME/.local/state}/mcpd"
sudo chown -R "$(id -u):$(id -g)" "$state"
find "$state" -type d -exec chmod 0700 {} +
find "$state" -type f -exec chmod 0600 {} +
```

If mcpd reports an unsafe parent, remove group and other write access from the specific parent that the diagnostic names. Do not change ownership or permissions on an unrelated system directory.

### Recover a corrupt file

A malformed or truncated `secrets.json` is a hard failure. Normal set and remove operations refuse to overwrite it. Stop mcpd, preserve the file for local diagnosis, move the active file aside, and let mcpd initialize a new snapshot:

```sh
state="${XDG_STATE_HOME:-$HOME/.local/state}/mcpd"
cp -p "$state/secrets.json" "$state/secrets.json.corrupt-copy"
mv "$state/secrets.json" "$state/secrets.json.disabled"
```

Restart mcpd, then set each required name again through the hidden CLI prompt or local panel. Keep both recovery files mode 0600. They contain credential material. Do not attach them to an issue or paste them into logs. Remove them after diagnosis and recovery are complete.

## Migration

Use this sequence to move from daemon environment variables to a provider:

1. Add the chosen `secrets` block and restart mcpd.
2. Set every referenced name through the hidden CLI prompt or local panel. The environment still wins during this step.
3. Run `mcpd secret status` and check the listed consumers. A set operation warns when the daemon environment shadows the stored value.
4. Remove the variables from the actual launch environment. For example, update the launchd agent, systemd user manager, or environment file that starts mcpd. Removing a value only from the current shell is not sufficient.
5. Restart mcpd. Confirm that each name reports `provider-present` and that the dependent backends reconnect.

Provider changes are also manual. mcpd does not copy values between native and file storage. Configure the destination, restart, and set each name again.

## Rollback

Restore the required variables to the daemon launch environment, remove the optional `secrets` block, and restart mcpd. This returns resolution to environment-only behavior. Provider entries remain intact and are not deleted automatically.

If rollback must also delete stored entries, remove them before you remove the `secrets` block. If the block is already gone, restore it temporarily, restart mcpd, remove the entries, then remove the block again.
