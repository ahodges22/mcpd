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

The generated unit passes a small baseline environment. If a backend depends on an environment
variable, add a persistent systemd drop-in and reinstall or restart the service:

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

The generated plist has automated rendering tests but has not yet completed a real-machine
launchd lifecycle test.

Check the same environment path before installing:

```sh
zsh -lc '[ -n "${YOUR_TOKEN_VARIABLE:-}" ]' \
  && echo 'token available' || echo 'token MISSING'
```

## Verification

`mcpd doctor` verifies that the declaration file loads, the user service is installed and
running, and the loopback dashboard answers successfully.
