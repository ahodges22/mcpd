## ADDED Requirements

### Requirement: The daemon is restarted automatically when it dies

The repository SHALL ship a service definition for each supported platform that restarts the
daemon unconditionally, including after an unclean kill and after a clean exit.

Every client on the machine points at one loopback address, so a daemon that is down leaves
every client with no MCP servers and no error to attribute it to. Restarting is the only thing
that keeps the concentration safe.

A restart SHALL NOT require re-authorising a backend that uses OAuth, because tokens are
persisted under the state directory rather than held only in memory.

#### Scenario: An unclean kill recovers on its own

- **WHEN** the daemon is killed with `SIGKILL`
- **THEN** it is running again without intervention, serving the same catalogue, with every
  backend that was serving before serving again

### Requirement: A supervised daemon is given the credentials its declarations reference

A service definition SHALL make available every environment variable the declaration file
references through `${VAR}`, and the documentation SHALL say that the list has to be extended
when a declaration references a new one.

An incomplete list fails quietly and the failure is misattributed. The daemon starts, reports
itself healthy and serves its endpoints, while each backend behind a bearer header fails its
handshake for what looks like a network or provider fault. This was a real defect in the
shipped unit: it named none of the four, which would have taken out nine of fourteen backends.

#### Scenario: A supervised daemon reaches the same state as a hand-started one

- **WHEN** the daemon is started by the service definition rather than by hand
- **THEN** the same number of backends are serving and the catalogue is the same size

### Requirement: The daemon is tied to the session that holds its credentials

A service definition SHALL start the daemon with the user's login session, and SHALL NOT start
it at boot.

A variable declared in a shell rc file exists only once a session has established it, so a
daemon started before that comes up without it. On Linux this means the unit is wanted by the
graphical session rather than by the user manager, whose own target starts at boot wherever
lingering is enabled. Lingering is frequently enabled for an unrelated service, and requiring it
to be turned off to make this daemon correct would trade one broken thing for another.

Variables declared under `~/.config/environment.d/` do not have this problem: a systemd user
environment generator reads them when the manager starts, session or not. A set that mixes the
two sources therefore fails *partially* at boot, which is harder to diagnose than failing
completely, because the backends that do work make the daemon look healthy. Declaring every
needed variable there is the durable fix, and binding to the session is what makes the daemon
correct until that is done.

Binding to the session SHALL NOT be documented as the mechanism that supplies an rc-declared
variable, because it is not. Activating the session target sources nothing. Such a variable
reaches the service only through a chain the daemon does not control: the session pushes its
environment into the user manager at login, and the shell profile has to have sourced the rc file
for it to be in that environment at all. Binding to the session only starts the daemon late
enough to benefit from that chain, so where the chain is absent the variable is missing either
way.

Where a platform or installation has no session to bind to, the documentation SHALL state the
consequence rather than leave the boot-time case looking supported.

#### Scenario: Lingering being enabled does not produce a credential-less daemon

- **WHEN** the user manager is configured to linger for an unrelated service
- **THEN** the daemon still starts with the session, not at boot

### Requirement: Platform verification and its boundary are documented

Each service definition SHALL be run on the platform it targets before the documentation calls
it verified. The recorded evidence SHALL cover the supervisor loading the shipped command and
restart settings, the daemon running under that supervisor, and the status surface reporting
the configured backends serving.

Any lifecycle boundary not exercised, such as a fresh-login or reboot-time start, SHALL remain
explicit rather than being implied by a successful session-time run.

#### Scenario: The macOS agent runs under launchd

- **WHEN** the shipped agent is loaded in the user's graphical launchd domain
- **THEN** launchd runs the shipped command and restart settings, the daemon remains running,
  and its status surface reports the configured backends serving

#### Scenario: The untested lifecycle boundary remains visible

- **WHEN** session-time verification has passed without a fresh login or reboot
- **THEN** the documentation identifies that remaining boundary rather than claiming it passed
