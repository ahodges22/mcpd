# Running mcpd as a user service

Use the CLI to install, inspect, start, or remove the service:

```sh
mcpd service install
mcpd service status
mcpd service start
mcpd service uninstall
```

The generated service records absolute binary, configuration, and state paths. Re-running
`mcpd service install` replaces the definition and restarts mcpd.

## Linux

mcpd installs `~/.config/systemd/user/mcpd.service` and enables it for
`graphical-session.target`. It starts with the login session rather than at boot because native
credential stores and environment credentials depend on that session. A headless host without a
graphical session needs a deliberate service policy instead of this default.

The generated unit passes a small baseline environment plus every `${VAR}` and `api_key_env`
name referenced by the declaration file. Re-run `mcpd service install` after adding a new
reference so the generated allowlist stays current.

If a stdio backend uses `env_passthrough` for a variable that is not also referenced through
`${VAR}`, add that name with a persistent systemd drop-in, then restart the service:

```sh
systemctl --user edit mcpd
```

```ini
[Service]
PassEnvironment=YOUR_TOKEN_VARIABLE
```

The variable must also exist in the user manager environment. Check without printing its value:

```sh
systemctl --user show-environment | grep -q '^YOUR_TOKEN_VARIABLE=' \
  && echo 'token available' || echo 'token MISSING'
```

## macOS

mcpd installs `~/Library/LaunchAgents/dev.mcpd.daemon.plist`. The agent starts mcpd through a
non-interactive zsh login shell so declarations can use exports from `.zprofile`. `.zshrc` is not
read in this mode. Logs go to `~/Library/Logs/mcpd.log`.

The generated agent has automated rendering tests and has run in a graphical user domain with
the shipped command, `RunAtLoad`, `KeepAlive`, and throttle settings. launchd kept the daemon
running with a clean last exit, and the loopback status surface reported every configured
backend serving. A fresh-login or reboot-time start has not been exercised.

Check the same environment path before installing:

```sh
zsh -lc '[ -n "${YOUR_TOKEN_VARIABLE:-}" ]' \
  && echo 'token available' || echo 'token MISSING'
```

## Optional status icon

Run the icon manually with `mcpd tray`. Use `mcpd tray --addr 127.0.0.1:PORT` only when the
daemon uses a non-default loopback port. The tray reads the loopback status API, never backend
credentials or declarations, and never starts or stops the daemon. Control-C and `Quit status
icon` both stop it cleanly.

The release archive includes opt-in startup definitions under `dist/`. They are separate from
the daemon service installed by `mcpd service install`.

On Linux, copy `dist/mcpd-tray.service` to `~/.config/systemd/user/`, then run:

```sh
systemctl --user daemon-reload
systemctl --user enable --now mcpd-tray.service
```

Dashboard and authorization actions require `xdg-open`. A desktop without a compatible
StatusNotifierItem host needs one before it can display the icon; the tray remains alive while
the host is absent.

On macOS, copy `dist/dev.mcpd.tray.plist` to `~/Library/LaunchAgents/`, then run:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.mcpd.tray.plist
```

The tray agent runs only in an Aqua session. Its non-login shell expands `HOME`, then replaces
itself with a credential-free tray process. It does not read login profiles.

Quit is an explicit request to leave the tray stopped for the current session. Start it again
with `systemctl --user start mcpd-tray.service` on Linux or `launchctl kickstart
gui/$(id -u)/dev.mcpd.tray` on macOS. Disable or boot out the unit to remove login startup.

## Verification

`mcpd doctor` verifies that the declaration file loads, the user service is installed and
running, and the loopback dashboard answers successfully.
