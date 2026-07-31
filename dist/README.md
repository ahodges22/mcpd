# Supervising the daemon

Four clients are pointed at `127.0.0.1:7420`. If the daemon is not running, every one of them
has no MCP servers at all, and the symptom is silence rather than an error. Supervise it.

Both platforms start the daemon **with the login session**, not at boot. This is deliberate.
The daemon holds the credentials its declarations reference through `${VAR}`, and those exist
only once a login session has established them. A boot-time start would come up with no tokens
and every backend behind a bearer header would fail its handshake while the daemon itself
looked healthy. Nothing needs a local MCP proxy when nobody is logged in.

## Linux, systemd user unit

```sh
mkdir -p ~/.config/systemd/user
cp dist/mcpd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mcpd
```

The unit is wanted by `graphical-session.target`, not `default.target`, and that is the whole
point. `default.target` is the user manager's own target, so with `loginctl enable-linger` set
the daemon would start at boot with no credentials. Lingering is often already on for some
unrelated service, and telling someone to turn it off to fix this daemon is the wrong trade.
Binding to the session sidesteps it entirely.

On a headless machine there is no graphical session, so use `WantedBy=default.target` there
and either leave lingering off or accept one `systemctl --user restart mcpd` after boot.

`PassEnvironment` carries variables from the user manager's environment. It must name every
variable a declaration references. A variable that is not set is simply not passed, so an
incomplete list fails quietly.

Check what the manager can actually pass, without printing any values:

```sh
for v in DD_ACCESS_TOKEN GITHUB_TOKEN LITELLM_KEY N8N_STAGE_MCP_TOKEN; do
  systemctl --user show-environment | grep -q "^$v=" && echo "$v ok" || echo "$v MISSING"
done
```

## macOS, launchd user agent

```sh
cp dist/dev.mcpd.daemon.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.mcpd.daemon.plist
```

`bootout` to remove it, `kickstart -k gui/$(id -u)/dev.mcpd.daemon` to restart it. The older
`launchctl load -w` still works but is deprecated.

launchd has no `PassEnvironment`, so the agent starts through a login shell and inherits the
exports. Put them where a **non-interactive** login shell reads them, which on zsh means
`.zprofile`. `.zshrc` typically returns early when the shell is not interactive, and the
exports are then skipped with no error. Check the same way:

```sh
for v in DD_ACCESS_TOKEN GITHUB_TOKEN LITELLM_KEY N8N_STAGE_MCP_TOKEN; do
  sh -lc "[ -n \"\${$v:-}\" ]" && echo "$v ok" || echo "$v MISSING"
done
```

Logs go to `~/Library/Logs/mcpd.log` and nothing rotates them.

## Verifying either one

The daemon is up when the status page answers and the backend count is what you expect:

```sh
curl -s http://127.0.0.1:7420/ | grep -o '[0-9]* backends'
```

Supervision is working when killing it uncleanly brings it back:

```sh
pkill -9 -x mcpd; sleep 5
curl -s http://127.0.0.1:7420/ | grep -o '[0-9]* backends'
```

A backend that needs OAuth does not need re-authorising after a restart. Tokens are persisted
under the state directory, so a restart reloads them.

## What is not covered

The macOS agent has not been run. It was written against launchd's documented behaviour and
reviewed, but every claim about it here is untested, unlike the systemd unit.
