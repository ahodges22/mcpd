## Context

See `proposal.md` for motivation. The daemon is a single point of availability for every local MCP client, but its backend credentials belong to the user's login environment. The supervision design therefore has to restart the daemon without widening credential access or starting it before those credentials are available.

Linux and macOS expose different user-service environment models. A systemd user service can copy selected variables from the user manager. A launchd agent receives a minimal environment and must enter the user's login-shell environment explicitly.

## Goals / Non-Goals

**Goals:**

- Keep the daemon running during the graphical login session and restart it after any exit.
- Deliver only the credential variables named by current backend declarations.
- Preserve OAuth sessions across daemon restarts through the existing persistent state.
- Provide install, verification, and rollback instructions for both supported platforms.

**Non-Goals:**

- Boot-without-session operation, socket activation, credential files, or `systemd-creds`.
- Automatic discovery of newly referenced environment variable names.
- Log rotation for the macOS agent.

## Decisions

### Bind Linux supervision to the graphical session

The systemd user unit is wanted by and part of `graphical-session.target`, with unconditional restart behavior. It names the declaration-referenced variables through `PassEnvironment`; an unset variable is not synthesized.

Starting from `default.target` was rejected because a lingering user manager can reach that target at boot. A mixed credential source then fails partially: variables from `environment.d` exist, while an rc-only variable does not. Requiring every credential in `environment.d` would support boot-without-session, but that is a separate operator decision outside this change.

### Use a login shell as the macOS environment adapter

The launchd agent runs `/bin/zsh -lc` and immediately `exec`s the daemon. A login shell is used because launchd has no `PassEnvironment` equivalent and supplies almost no user environment. `exec` leaves launchd supervising the daemon itself rather than a wrapper process.

`KeepAlive` and `RunAtLoad` provide unconditional session-time availability. `ThrottleInterval` bounds restart frequency when configuration prevents startup. Shell redirection writes the daemon log under the user's Library because launchd does not expand `$HOME` in `StandardOutPath`.

An environment file was rejected because it would duplicate credentials into another persistent location. A hard-coded home directory was rejected because the distributed agent has to work for any user.

### Keep verification explicit and credential-safe

Installation documentation checks variable presence without printing values, compares supervised and hand-started backend state, verifies restart recovery, and records platform-specific evidence. The service definitions do not inspect or copy credential values themselves.

## Risks / Trade-offs

- [The named Linux environment list can drift from declarations] -> Document that operators must extend `PassEnvironment` when a declaration references a new variable.
- [A login shell may not read where an operator placed exports] -> Document the zsh profile boundary and provide a non-printing presence check.
- [Session binding does not itself import an rc file] -> State the actual environment-import chain and avoid claiming that the target supplies credentials.
- [A broken declaration can cause repeated restarts] -> Apply the platform restart throttle and keep diagnostics in the service logs.
- [The macOS log grows without rotation] -> Document the limitation rather than adding a new logging subsystem.

## Migration Plan

1. Install the platform service definition and verify its parsed command and restart settings.
2. Stop any hand-started daemon before enabling the user service so the loopback port has one owner.
3. Verify credential presence without printing values, then compare backend and catalogue state.
4. Confirm persisted OAuth sessions recover and an unclean kill restarts automatically.
5. Roll back by unloading or disabling the user service; configuration and daemon state remain untouched.
