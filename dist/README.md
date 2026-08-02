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

`PassEnvironment` carries variables from the user manager's environment. Edit the unit before
you install it and name every variable that a declaration references. A variable that is not
set is simply not passed, so an incomplete list fails quietly.

Note what binding to the session does and does not do. Activating `graphical-session.target`
does **not** source a shell rc file or import anything by itself. A variable declared in
`~/.bashrc` reaches the user manager only because the desktop session pushes its own
environment in at login, with `dbus-update-activation-environment` or
`systemctl --user import-environment`, and because `~/.profile` sources `~/.bashrc` so the
variable is in the session environment to begin with. That import happens during session
startup, before the target activates, which is what makes the ordering work.

So the session binding is not itself the mechanism, it just starts the daemon late enough to
benefit from one. On a machine whose session does not import its environment, or whose
`~/.profile` does not chain to `~/.bashrc`, an rc-only variable is missing either way. Declaring
it in `~/.config/environment.d/` is the only version of this that does not depend on a chain of
session behaviour.

Check what the manager can actually pass, without printing its value:

```sh
systemctl --user show-environment | grep -q '^YOUR_TOKEN_VARIABLE=' \
  && echo 'token available' || echo 'token MISSING'
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
zsh -lc '[ -n "${YOUR_TOKEN_VARIABLE:-}" ]' \
  && echo 'token available' || echo 'token MISSING'
```

The agent starts the daemon through `/bin/zsh -lc` for the same reason the check above uses
`zsh -lc`: only a zsh login shell reads `.zprofile`. `/bin/sh` in login mode reads `~/.profile`
instead and would skip the exports silently.

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
