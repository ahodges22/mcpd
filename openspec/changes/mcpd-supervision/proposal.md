## Why

All four clients on this machine are now pointed at `127.0.0.1:7420`. That concentration is
the point of the daemon, and it is also a new single point of failure: if the daemon is not
running, every client has no MCP servers at all, and the symptom is an empty tool list rather
than an error anyone would connect to a dead proxy.

Until now the daemon was started by hand. It survived neither a crash nor a reboot, and the
repository shipped a systemd unit that had never been installed. Reviewing it against a live
machine found a defect that would have made installing it worse than not:

- `PassEnvironment` named `HOME`, `USER` and similar, but none of the four variables the
  declarations reference through `${VAR}`. The daemon would have started with no credentials,
  and every backend behind a bearer header would have failed its handshake while the daemon
  itself reported healthy. Nine of fourteen backends.
- The unit was wanted by `default.target`, which is the user manager's own target. This machine
  has `loginctl enable-linger` set for an unrelated service, so the daemon would have started
  at boot, before any session had established the credentials it needs.

macOS was not covered at all.

## What changes

- Name the credential variables the declarations reference, so a supervised daemon has them.
- Bind the unit to `graphical-session.target` rather than the user manager, so it starts with
  the session that owns the credentials and is unaffected by lingering being on for something
  else.
- Add a launchd user agent for macOS. launchd has no `PassEnvironment` and hands an agent
  almost no environment, so it starts the daemon through a login shell instead. This is the one
  real difference between the two platforms.
- Document both, including how to check that the credentials are reachable without printing
  any of them, and that a restart does not require re-authorising an OAuth backend.

## Scope / Out of scope

In scope: the two service definitions, their installation, and verifying on Linux that a
supervised daemon reaches the same state as a hand-started one and recovers from `SIGKILL`.

Out of scope: making the daemon exit when a referenced variable is unset, reading credentials
from a file or a keychain rather than the environment, `systemd-creds`, log rotation for the
macOS agent, socket activation, and running the daemon at boot with no session. Running it with
no session would need credentials from somewhere other than a session, which is a different
change with a different threat model.

## Impact

The daemon becomes something the machine keeps running rather than something the user
remembers to start. The blast radius of it being down is unchanged and remains total, which is
why this is worth doing rather than deferring.

The macOS agent is written against launchd's documented behaviour and is **untested**. It is
committed as such and labelled that way, because a plausible service definition nobody has run
is not the same as a working one.
