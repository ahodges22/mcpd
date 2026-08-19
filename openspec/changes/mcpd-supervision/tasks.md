## 1. Review the shipped unit against a live machine

- [x] 1.1 Establish how the daemon resolves credentials. `${VAR}` in headers and
      `api_key_env` resolve against the daemon's **own** process environment, so a supervised
      daemon must be given them; a user unit inherits nothing from an interactive shell
- [x] 1.2 Determine which variables the declaration file references, by name only, without
      reading any value: `DD_ACCESS_TOKEN`, `GITHUB_TOKEN`, `GATEWAY_TOKEN`, `SERVICE_TOKEN`
- [x] 1.3 Test both candidate mechanisms by execution, printing presence and never a value.
      Both the user manager environment and a non-interactive login shell carry all four, so no
      new credential file is needed and the existing unit's decision to avoid one stands
- [x] 1.4 **Defect: the unit named none of the four in `PassEnvironment`.** Installing it as
      shipped would have started a daemon reporting itself healthy with nine of fourteen
      backends failing their handshakes
- [x] 1.5 **Defect: `WantedBy=default.target` with lingering enabled starts the daemon at
      boot**, before a session has established any credentials. Lingering is on for an
      unrelated service on this machine, so turning it off was not an acceptable fix
- [x] 1.6 Correct 1.5's reasoning, which was first recorded as "no credentials at boot" and is
      not what happens. Three of the four are declared in `~/.config/environment.d/`, which a
      user environment generator reads at manager start with no session involved. Only
      `GITHUB_TOKEN` is rc-only. **The boot failure is partial, not total: `github` alone, 81
      tools, which reads as GitHub being down rather than as a local misconfiguration.** The
      vault already held this lesson under `references/gui-launch-env-datadog-mcp-auth`, where
      the same rc-versus-`environment.d` split broke Datadog header auth; consulting it before
      the analysis rather than after would have got this right first time
- [x] 1.8 Adversarial review raised that `PassEnvironment` only copies what is already in the
      user manager, and that entering `graphical-session.target` sources nothing by itself. Both
      true. Verified the actual chain by execution: `~/.profile` sources `~/.bashrc`, so the
      variable is in the session environment, and the session pushes that into the user manager
      at login, which is ordered before the target activates. **The conclusion holds and the
      documentation did not: it implied the session binding supplies the variable.** Corrected in
      `docs/service.md` and the requirement. A fresh-login start is still unverified, only a
      mid-session start and a kill-recovery were
- [ ] 1.7 Durable fix, left to the user because it means writing a credential: declare
      `GITHUB_TOKEN` in `~/.config/environment.d/` at mode 600. It is a distinct secret, not a
      copy of the `GH_PAT` already declared there. Once it is there the unit can go back to
      `WantedBy=default.target` and the daemon no longer needs a session at all

## 2. Fix Linux and add macOS

- [x] 2.1 Name the four credential variables, with a comment that the list has to grow with the
      declarations and that an unset variable is simply not passed
- [x] 2.2 Bind the unit to `graphical-session.target` with `PartOf=`, so it starts and stops
      with the session that owns the credentials and lingering becomes irrelevant
- [x] 2.3 Add a launchd user agent. launchd has no `PassEnvironment`, so it starts the daemon
      through `sh -lc` and inherits the profile's exports. `exec` keeps the shell out of the
      process tree so `KeepAlive` watches the daemon; `ThrottleInterval` stops a bad
      declaration file becoming a respawn spin; the log redirection is in the shell because
      launchd does not expand `$HOME` in a path value
- [x] 2.4 Document both, including the presence checks, the headless alternative, the zsh
      `.zprofile` versus `.zshrc` trap, and that no rotation exists for the macOS log

## 3. Verify on Linux

- [x] 3.1 Install, stop the hand-started daemon, enable the unit. **611 tools and 14 of 14
      serving, so `PassEnvironment` delivered the credentials.** Both stdio children started
      inside the unit's cgroup, so the explicit `PATH` is right
- [x] 3.2 OAuth backends recovered from persisted tokens with no re-authorisation
- [x] 3.3 `SIGKILL` the daemon: it returned on its own with the same 611 tools and 14 serving.
      Observed incidentally on first start too, where the hand-started process still held the
      port and `Restart=always` recovered the bind on the second attempt
- [x] 3.4 Confirm the enable symlink moved to `graphical-session.target.wants` and that
      `PartOf`, `WantedBy` and `Restart` read back as intended

## 4. Not done

- [ ] 4.1 Run the launchd agent on macOS. It is committed untested and labelled as such in
      `docs/service.md`. Nothing here has executed it, and a reboot-time start on either
      platform is likewise unverified: only a session-time start and a kill-recovery were
