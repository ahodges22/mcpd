## 1. Provider feasibility gate

- [x] 1.1 Initialise the Go module and add the MCP SDK
- [x] 1.2 Confirm `oauthex` signatures with `go doc` before writing code against them
- [x] 1.3 Write `cmd/oauthprobe`: discovery from the 401 challenge, protected-resource
      metadata, authorization-server metadata, and opt-in DCR with the loopback redirect
- [x] 1.4 Run it against real Notion and record the outcome in `PHASE0.md`
- [x] 1.5 Take the gate decision. **Result: discovery, DCR, and loopback all work; public
      client with no secret. Proceed as designed**

## 2. Config and least-privilege child environments

- [x] 2.1 Write the failing tests: a declared passthrough grants `AWS_*` and `KUBECONFIG`;
      a backend declaring nothing receives no credentials; the constructed environment is
      never the unset value
- [x] 2.2 Implement `internal/config`: loading, validation, `${VAR}` expansion, curated
      base plus declared `env` plus declared `env_passthrough`
- [x] 2.3 Migrate the prototype's backend list to `testdata/config.example.json`, using
      placeholder hostnames, `${GH_PAT}` rather than a shell-only variable, and
      `env_passthrough` for the infrastructure CLI backend
- [x] 2.4 Tests green, then commit

## 3. Backend sessions and at-most-once dispatch

- [x] 3.1 Write `internal/testfake`: an in-process MCP backend counting list calls and side
      effects, with a hook to hold a list in flight
- [x] 3.2 Write the failing tests: a call reaches the owning backend; a backend that commits
      a side effect then drops the connection yields exactly one side effect and an
      outcome-unknown error; a write that errors after delivery does not replay
- [x] 3.3 Implement `internal/backend/backend.go`: transport construction with explicit
      `cmd.Env`, reconnect with backoff, health record, and the `RWMutex` dispatch gate
- [x] 3.4 Implement `internal/backend/registry.go`: routing, lifecycle mutex, generation
      counter, and the tool-list-changed callback
- [x] 3.5 Tests green under `-race`, then commit

## 4. Catalog with coalescing refresh

- [x] 4.1 Write the failing tests: a notification during an in-flight list causes a second
      read whose result wins; a trigger during that second read causes a third; a burst
      within the debounce window yields one follow-up; one failing backend does not sink
      the catalog
- [x] 4.2 Implement canonical ids, flattening, and persistence
- [x] 4.3 Implement the trigger-counter loop with debounce and backoff, and generation
      rejection on commit
- [x] 4.4 Tests green under `-race -count=5`, then commit

## 5. Lifecycle serialization and the kill switch

- [x] 5.1 Write the failing tests: disable beats an in-flight refresh; a pending retry does
      not respawn a disabled backend; a dispatch never writes after the gate closes,
      asserted on the fake's received-request log rather than the caller's return value; an
      override survives a restart
- [x] 5.2 Implement override persistence
- [x] 5.3 Implement disable in order: persist the override, close and drain the gate, cancel
      and await outstanding tasks, close the session and terminate the child, bump the
      generation
- [x] 5.4 Implement enable under the same lock so a re-enable cannot race a teardown and
      leak a second child
- [x] 5.5 Tests green under `-race -count=5`, then commit

## 6. Embeddings and rank fusion

- [x] 6.1 Write the failing tests: fusion degrades to lexical with no vectors; ranks are
      fused rather than scores; an unreachable embeddings service is soft
- [x] 6.2 Port the prototype's lexical scorer, including its tool-name squashing
- [x] 6.3 Implement the embeddings client with batching and a content-hash cache
- [x] 6.4 Implement reciprocal rank fusion
- [x] 6.5 Tests green, then commit

## 7. Abstention signal

- [x] 7.1 Write the failing tests: overlapping evidence yields disabled abstention and an
      error naming the overlap; separated evidence yields a threshold inside the gap
- [x] 7.2 Implement the fixed selection rule against raw cosine and lexical evidence, never
      the fused score
- [x] 7.3 Tests green, then commit

## 8. The two MCP endpoints

- [x] 8.1 Write the failing tests: the facade advertises exactly three tools; pass-through
      advertises canonical names; a call reaches the owning backend; an empty catalog is
      explained rather than silently empty
- [x] 8.2 Implement the facade server with `search_tools`, `describe_tool`, `call_tool`
- [x] 8.3 Implement the pass-through server, syncing tools on catalog change so the SDK
      emits tool-list-changed
- [x] 8.4 Tests green, then commit

## 9. Guard, status page, and inspector

- [x] 9.1 Write the failing tests: a foreign origin is rejected on both an MCP endpoint and
      the status API; an absent origin is accepted; a mutation is rejected on GET; a tool
      result containing markup renders as literal text
- [x] 9.2 Implement the guard: one shared cross-origin protection value, POST-only
      mutations, the callback exemption, and a deny handler carrying a reason
- [x] 9.3 Implement the status API and the embedded page, inserting all dynamic values as
      text
- [x] 9.4 Implement the inspector, rendering results as text and surfacing destructive
      annotations
- [x] 9.5 Verify no markup-insertion API appears anywhere in the assets
- [x] 9.6 Tests green, then commit

## 10. Downstream OAuth

- [x] 10.1 Confirm the authorization-handler config field names with `go doc`
- [x] 10.2 Write the failing tests: the full flow against a fake provider through
      registration, exchange, 0600 persistence, authenticated reconnect, restart reuse, and
      refresh; a mismatched or replayed state is rejected with no token written, driven as a
      browser-shaped GET through the real guard
- [x] 10.3 Implement the store: pending-authorization registry, code fetcher, atomic
      persistence at 0600 inside a 0700 directory, and write-back on refresh
- [x] 10.4 Wire the handler into the backend HTTP client using dynamic client registration,
      per the Phase 0 finding, and return a backend to needs-auth on refresh failure
- [x] 10.5 Add the authorize and callback routes
- [x] 10.6 Tests green under `-race`, then commit

## 11. Daemon entrypoint and first real run

- [x] 11.1 Write the failing test: all three surfaces answer, and the two MCP endpoints
      advertise different tool counts
- [x] 11.2 Implement `cmd/mcpd serve`: one mux, both MCP handlers wrapped in
      `guard.Protect` so they share the guard's protection value, overrides loaded before
      connecting, graceful shutdown draining and terminating children. Shutdown is
      `backend.Registry.Shutdown()`, which drains the dispatch gate, stops each refresh,
      closes each session and terminates each child, writes no override and evicts no
      tools, and after which every transition and every dial refuses with
      `backend.ErrShutdown`, so a detached `reconnect-all` cannot respawn a child. Cancel the
      context given to `catalog.Start` before calling it anyway, so no loop keeps running
      against a registry that refuses it. `Shutdown` takes no context and can take none:
      draining the dispatch gate has no cancellable variant, so one `tools/call` on a backend
      with no configured `timeout` blocks process exit until the client gives up. That is the
      known unbounded-call limitation surfacing in a new place; decide deliberately whether
      to bound the shutdown from outside and exit regardless. Must call `catalog.Load()` at
      startup (it is what backs the spec's persistence requirement) and one startup
      `catalog.RefreshAll()`, because `catalog.Start` deliberately performs no immediate
      refresh to avoid doubling every cold start's reads
- [x] 11.2a Immediately after `catalog.Load()`, call `catalog.Drop(name)` for every backend
      that loaded disabled, and pin it with a test. A crash between Task 5's override write
      and its tool eviction leaves that backend's tools in the persisted catalog, and a
      disabled backend is never re-listed, so this is the only thing that ever removes
      them. It is a step of its own because a paragraph is skippable and this is not
- [x] 11.3 Wire embeddings into ranking. This is a multi-package code change, not wiring:
      nothing outside `internal/embedding` imports it today, so every seam below has to be
      built. Until it lands, fusion degenerates to lexical-only and abstention is provably
      inert (`qvec` always nil, so `HasCosine` is always false), which makes Tasks 6 and 7
      dead code in production. The obligations, each of which is missing:
      (a) config gains the gateway URL, the API key and the model, and
      `embedding.NewClient(baseURL, apiKey)` has no way to take a model: its `model` field
      is private and defaulted, so the constructor has to accept one;
      (b) `mcpsrv.NewSearch(cat, reg, th)` accepts no embedding client and no vector
      source, and `searchHandler` hardcodes `rank.Fuse(in.Query, entries, nil, nil, limit)`.
      Both the per-entry vectors and the query vector have to reach that call, so
      `internal/mcpsrv`'s API changes;
      (c) the query vector needs a gateway call at search time, and it must fail soft: a
      failed query embed passes nil to `rank.Fuse` and the search still answers, exactly as
      `Vectorize` degrades a failed catalog embed to lexical-only rather than erroring;
      (d) nothing ever calls `Cache.Load` or `Cache.Save`, so the spec's requirement that a
      warm cache works offline has no data path: `Load` at startup, `Save` after each
      `Vectorize`. `Vectorize` belongs at catalog-refresh time, and the natural place is
      11.4's post-commit hook, which fires from each refresh goroutine, so several
      `Vectorize` calls can run at once. `Cache` is safe for that. Note the hook also fires
      from `Drop` inside a lifecycle teardown, with that backend's dispatch gate held closed
      (see 11.4), so a gateway call wired to it delays every disable by its own timeout, and
      anything that waits on a tool call there deadlocks the daemon permanently;
      (e) the tool-search spec's "the status surface reports how many tools are
      unvectorized" has no field anywhere. `Vectorize` returns the count, but neither
      `backend.Health` nor `web.statusView` can carry it, so one of them gains a field and
      the template and the JSON render it.
      For the shape of the construction, copy `internal/web/helpers_test.go:39` and
      `internal/oauthstore/flow_test.go:55`: between them they stand up every piece of
      production wiring, including the late-bound closures the registry/catalog/store cycle
      needs, and they are the only place that wiring is written down
- [x] 11.4 Wire `mcpsrv.Passthrough.Sync` to the catalog's post-commit hook,
      `catalog.OnCommit`, which fires outside the catalog mutex on all three mutation paths
      (`commit`, `exclude`, `Drop`). The existing tool-list-changed hook fires pre-commit and
      so would sync against stale entries, and `Sync` holds its own lock across
      `cat.Entries()`. Without this, nothing calls `Sync` and the pass-through tool set never
      changes after startup. Register it before the first refresh starts: the field is not
      guarded, because it is written once at construction. **One of the three paths is not a
      refresh:** `Drop` is called from inside `Backend.teardown`, which holds that backend's
      dispatch gate closed until it returns, so the hook can run inside a lifecycle
      transition. `Sync` is safe there (it takes only its own lock, and reaches the catalog
      solely through `Entries`), but a hook that waits on a tool call **deadlocks the daemon
      permanently, not just slowly**: the dispatch lease it needs is the gate the teardown is
      holding, and that teardown is waiting for the hook to return. Construct the
      pass-through after `catalog.Load()`, because `NewPassthrough` syncs in its constructor
      and would otherwise
      serve an empty tool set until the first refresh commits
- [x] 11.5 Wire all four catalog refresh triggers: `catalog.Start(ctx)` covers TTL expiry, and
      `backend.Hooks{ToolListChanged, Reconnected}` both point at `Catalog.Trigger`. The
      mechanisms are built in Tasks 3 and 4; only the wiring is left, and an unwired hook is a
      silently stale catalog. Task 5 added three more hooks that must also be wired, or a
      disable stops being a kill switch: `StopRefresh` to `Catalog.StopRefresh`, `DropTools`
      to `Catalog.Drop`, and `Refresh` to `Catalog.Trigger`. `NewRegistry` now also takes the
      `*backend.Overrides` loaded from the state directory. Task 10 added two more hooks that
      tasks.md never wrote down and that only the test harnesses
      (`internal/web/helpers_test.go:39` and `internal/oauthstore/flow_test.go:55`) show:
      `backend.Hooks.AuthHandler` must be `store.Handler`, and `oauthstore.New` must receive
      `Hooks{NeedsAuth: b.NoteNeedsAuth, Authorized: b.NoteAuthorized}` routed through the
      registry. A nil `AuthHandler` fails loudly at the first dial of an OAuth backend; nil
      store hooks fail **silently**, and needs-auth then never surfaces at all, which is the
      worse half. Copy both harnesses rather than deriving the wiring again
- [x] 11.6 Write the systemd user unit, relying on the existing user environment import
      rather than an environment file
- [x] 11.7 Build, install, enable, and confirm the status endpoint answers with backends up.
      A backend that is down here is a missing passthrough variable, to be added rather than
      worked around by inheriting everything
- [x] 11.8 Commit

## 12. Client wiring

- [ ] 12.1 Write the failing tests, with golden inputs for all four clients: an exact round
      trip with no intervening edits; approval blocks migrated and still active; an
      unrelated later edit surviving revert; revert refusing on a hand-modified owned region
- [ ] 12.2 Implement the four per-client writers and endpoint selection
- [ ] 12.3 Implement surgical revert against current file content, refusing on conflict
- [ ] 12.4 Tests green, then dry-run against the real configurations before applying, one
      client at a time, confirming each still starts
- [ ] 12.5 Commit

## 13. Ranking eval and calibration

- [ ] 13.1 Port the prototype's queries verbatim as a regression baseline and convert
      answers to acceptable sets
- [ ] 13.2 Expand to roughly 36 answerable queries across paraphrase, near-name, and
      cross-backend-ambiguous categories, marking roughly ten held out and written before
      any tuning
- [ ] 13.3 Write the separate negative calibration and negative validation query sets
- [ ] 13.4 Implement `cmd/evalrank`, exiting non-zero when any expected tool is absent
      rather than scoring a shrunken denominator, and asserting the catalog is fully
      vectorized before calibrating: a partially warm cache reports comparable cosine
      evidence over only a subset, biasing the answerable floor down and potentially
      erasing a real gap
- [ ] 13.5 Include the embedding model and dimension in the cache key, and decide the
      on-disk format change and pruning that implies. A calibrated cosine threshold is only
      valid for the model that produced the vectors, so a model swap behind an unchanged key
      silently invalidates it. Complementary to Task 7's comparability check, which catches
      a gateway remap behind an unchanged model name but cannot catch a model change at
      unchanged dimension
- [ ] 13.6 Record the baseline, then calibrate thresholds and score the validation set
      exactly once
- [ ] 13.7 Iterate on fusion only if the gate fails, watching the held-out versus tuned gap
- [ ] 13.8 Commit

## 14. Live provider acceptance

- [ ] 14.1 Clear the stored token and confirm the backend reports needs-auth with a pending
      URL. The file is `oauth-notion.json` directly under the state directory: deleting
      anything else clears nothing, and then 14.4's restart-reuse check proves nothing
- [ ] 14.2 Complete authorization in the browser and confirm the backend connects with a
      non-zero tool count
- [ ] 14.3 Make one real authenticated call through the inspector
- [ ] 14.4 Restart the daemon and confirm the token is reused with no re-authorization
- [ ] 14.5 Force access-token expiry and confirm a refresh rather than a return to needs-auth
- [ ] 14.6 Append the outcome to `PHASE0.md` and commit


## 15. Backend management from the status surface

Depends on Task 11, because there is nothing to add a backend to until the daemon runs, and
on 11.4, because an added or removed backend must reach connected pass-through clients
through the catalog's post-commit hook.

This section went through seven adversarial review rounds. Several steps below say what an
earlier draft did and why it was wrong; those notes are load-bearing, because each of those
drafts looked correct.

The lock order this section introduces, outermost first: the operation lock, the config writer
mutex, the registry read-write lock, then the four existing backend levels (transition, gate,
life, mu), then the declared-set lock, then the state-store mutexes. The writer mutex is never
held across a registry mutation or a teardown, and the registry write lock is never held across
a teardown.

One refinement matters for 15.12a: the registry lock is never held while any backend lock is
acquired, so no path holds it and then reaches for a backend lock. That is what makes the reverse
safe, and a reload replacement needs the reverse, because it holds one backend's transition lock
across its own map mutation to keep a concurrent enable or disable out.

- [x] 15.1 Add one shared backend-name validator, `^[a-z0-9][a-z0-9_-]{0,63}$`. The name
      becomes a URL path segment, the `oauth-<name>.json` file name under the state directory,
      and the prefix of every `mcp__<server>__<tool>` id, so it has three injection surfaces and
      until now was trusted because only a hand-edited file could supply it. Call it from
      `config.Load`, from the add route, and from every existing and new route that takes a
      `{name}` path value, before any registry, override, filesystem or OAuth operation. The
      remove route matters most, because it is the one that deletes a name-derived file. Confirm
      all 13 currently declared names still load
- [x] 15.2 Write the failing tests for the config write path in `internal/config`: an unmodelled
      field survives an add; a file changed on disk since load refuses the write and leaves the
      file's content byte identical; an edit landing after the staleness comparison is
      recoverable from an archive and survives ten later routine writes; an unsupported exchange
      refuses the write; a second write after a successful one is not refused; a failing archive
      still commits, reports a warning, and leaves the displaced file at 0600; a 0644 file and
      its archive are both 0600 after a write
- [x] 15.3 Implement the config write path as one type owning the path, the baseline bytes, the
      declared-set snapshot and a mutex
- [x] 15.3a Read-modify-write over a preserving representation: decode the document and its
      `backends` object as `map[string]json.RawMessage` and re-encode only the entry that
      changed, so no unmodelled field is dropped. Marshalling a Go map sorts its keys, so the
      first write reformats the file into sorted order; that is accepted, and the archives are
      the recovery path
- [x] 15.3b Hold the writer mutex across the whole sequence, in this order: re-read the file,
      compare against the baseline, run the duplicate or existence check against the freshly
      read content, marshal, write the temp file, exchange, restrict the displaced inode, archive,
      adopt the written bytes as the new baseline, refresh the declared-set snapshot. Every step
      before the exchange is a read or a check, so a failure at any of them leaves nothing mutated.
      Adopting the new baseline is not optional: without it the first write poisons every later one
      into a permanent false refusal
- [x] 15.3c Commit with `unix.Renameat2(RENAME_EXCHANGE)` swapping the temp file and the
      configuration file, then archive the displaced content from the temp path. A copy taken
      before the swap would miss an edit that landed in between, which is exactly the window
      that matters. `golang.org/x/sys` is already an indirect dependency, so this is a promotion
      to direct. On `EINVAL` or `ENOSYS`, refuse the write and change nothing. **There is no
      plain-rename fallback**, because a fallback reintroduces the destruction window precisely
      where nobody would notice
- [x] 15.3d Ensure the resolved declaration directory is 0700 at startup and refuse to write
      declarations if it cannot be. That is the control, and putting it at startup is what keeps it
      compatible with 15.3e: a `chmod` can fail, so a guarantee resting on one would force a choice
      between aborting after the commit and knowingly keeping a readable credential
- [x] 15.3da Restrict the displaced inode to 0600 immediately after the exchange and before
      archiving, as defence in depth. A failure here is a warning: the file stays inside the 0700
      directory and is NOT deleted, so neither confidentiality nor recoverability is given up. Do
      NOT tighten the existing file before the exchange instead: an earlier draft did, and it is
      defeatable, because a hand editor can atomically replace the file with a fresh permissive
      inode after the check, so the mode of the inode you checked is not the mode of the inode you
      displace. Failing test first: replace the file with a 0644 inode between the daemon's read and
      its commit, force an archive failure, and assert the displaced file is owner-only
- [x] 15.3db Resolve the declaration path through symlinks once at startup and use the resolved
      path for every read and write. A config symlinked into a dotfiles repo is a normal setup, and
      `RENAME_EXCHANGE` on the link swaps the LINK for a regular file: the daemon would then read a
      detached copy while the user kept editing the original, and a mode change would follow the
      link to a file it was never asked to touch. Failing test first, with a symlinked config:
      after a write the link is still a link, its target holds the new content, and no unrelated
      file changed mode
- [x] 15.3e Split the return: an error for anything before the exchange, warnings for everything
      after it. Archiving and retention are post-exchange, so neither may fail the operation, and
      no caller may make its later steps conditional on them. If archiving fails, leave the
      displaced file in place under its temporary name and say so in the warning. Return a
      distinct sentinel error for a staleness mismatch and another for a duplicate name, so the
      route can answer 409 for each
- [x] 15.3f Archive to a numbered file beside the configuration. Compare the displaced bytes
      against the baseline: equal means the daemon displaced its own write, so the archive is
      routine and the ten most recent routine archives are retained; unequal means it displaced
      something it did not write, which is the only irreplaceable class, so retain it without
      limit. Create every archive at 0600 and tighten an existing one that is more permissive
- [x] 15.3g Own the declared-set snapshot behind its own lock: a declared check, the declaration
      identity for a name, and an explicit acquire/release pair so a state writer can hold the
      read lock across its own file replacement. Refresh it inside the writer mutex on every
      successful write and on reload adoption, and expose the write-lock drop that a removal or
      reload calls before cleanup. Also expose a read-validate-adopt entry point for 15.12
- [x] 15.4 Write the failing tests for runtime registration: an added backend serves a tool with
      no restart; a removed backend's session closes, its stdio child exits and its tools leave
      the catalog; a reload adopts a hand-added declaration and tears down a hand-removed one; a
      reload that changes one backend leaves every other backend's session, child and
      authorization intact
- [x] 15.5 Implement `Registry.Add` and `Registry.Remove`. Add an RWMutex and take it for reading
      in `Get`, `Names` and `Health`, which read the map and slice with no lock today because
      nothing ever mutated them after construction. Never hold it across a teardown: `Remove`
      takes it, deletes the entry, releases it, and only then tears the backend down, because
      teardown blocks on in-flight work. Tear down with the terminal `forShutdown` latch so
      nothing can respawn a child for a backend that is no longer declared; the latch is per
      object, so a later `Add` of the same name is unaffected. Neither method returns an error,
      which is what keeps the config write as the only commit point. `NewRegistry` must retain
      the `Hooks` value it was given, because `Add` needs it
- [x] 15.5a `Add` takes the initial enabled state as a parameter and never consults the override
      store, unlike `NewRegistry`. This needs a one-line comment, because the asymmetry with
      construction is otherwise indistinguishable from a bug. Reading the store inside `Add`
      would keep a reload replacement's disabled state and break a fresh add over a stale flag;
      taking it from the caller satisfies both
- [x] 15.5b Bring `Registry.Shutdown` under the same lock and latch the registry against later
      publication. `Shutdown` walks `r.names` and `r.backends` with no lock today, which was safe
      only because nothing mutated them after construction; 15.5 makes them mutable. Take the
      registry lock to snapshot the set, set a registry-level shutdown latch, and hold the
      operation lock so a mutation cannot interleave. The existing per-backend `forShutdown` latch
      does NOT cover this: the dangerous case is a backend that was not in the map when shutdown
      read it, whose stdio child then outlives the daemon. Failing test first, run under `-race`:
      an add concurrent with shutdown is refused, nothing races, and no child survives
- [x] 15.5c All three operations check that latch before they mutate anything, and for the add
      that means BEFORE its commit point, not inside `Registry.Add`. Refusing at registration would
      leave the declaration committed and the registry without it, and would make `Add` fallible,
      breaking 15.5's guarantee. Reload is not an exception: it registers backends exactly as an add
      does, so a reload queued behind shutdown would otherwise publish them and trigger a refresh
      that spawns a stdio child after shutdown had already walked the registry. Test both
      linearizations: reload first and shutdown tears down what it added, or shutdown first and
      reload refuses
- [x] 15.6 Write the failing tests for the transaction boundary, for serialization, and for the
      enabled-state hand-off: a refused config write leaves the registry, the running backend,
      the override and the stored token untouched; an add rejected as a duplicate leaves the
      existing backend's token and override untouched; an add rejected because the exchange is
      unavailable leaves both untouched; a backend added from the surface under a name carrying a
      disabled flag starts enabled and is still enabled after a simulated restart; a disabled
      backend whose declaration is edited and reloaded is still disabled and its stdio child is
      not started; an add whose name carries a matching stored token does not authenticate with
      it; a post-commit cleanup failure still removes the backend and still reports the failure;
      a removal and an add of the same name issued concurrently leave the declaration and the
      runtime agreeing
- [x] 15.7 Implement the operation lock: one mutex, taken by add, remove and reload, held across
      the config write, the registry mutation, the teardown and the cleanup. Document in one line
      why it extends past the commit point. Ending it at the write is a real race: a removal
      commits, a concurrent add of the same name commits and registers, and the removal's cleanup
      then deletes the new backend's registry entry, tools, override and token. That is the race
      15.6's concurrency test pins
- [x] 15.8 Implement the declared-set protocol on the two state writers the operation lock cannot
      cover. `Overrides.set` and the persisting token source each take the declared-set read
      lock, check that the backend is declared and that the identity matches, perform their file
      replacement, and only then release. A removal or reload takes the write lock and drops the
      name before it begins cleanup. Checking and releasing before writing leaves a
      time-of-check-to-time-of-use gap a removal can land inside; the property needed is "it was
      still declared when this write landed, so the cleanup that follows will see it". Failing
      tests first: an override write racing a removal either lands before the cleanup and is
      deleted by it or is refused, and never survives it; a token refresh completing after
      removal leaves no record
- [x] 15.8a Comment at each site why the operation lock is NOT used there, naming the inversion:
      a token refresh persists from inside `oauth2`'s `Token()`, which runs underneath the
      backend `life` lock, so taking the outermost operation lock there can deadlock against a
      removal holding it while waiting on that same backend's teardown. Comment on the
      acquire/release pair why the lock spans the write rather than just the check
- [x] 15.9 Implement the add operation: validate the name and the declaration, then hand off to
      the writer, which performs the fresh read, the staleness check, the duplicate check and the
      commit. Nothing is deleted before the exchange. After it succeeds, in this order: attempt
      the hygiene deletion of any state found under the name, then `Registry.Add` with enabled
      state true, then trigger a catalog refresh. The deletion precedes registration because a
      registered backend is immediately routable and could dial and authenticate with the record
      being deleted. None of the three is conditional on the writer's warnings, and a deletion
      failure is reported without stopping the other two. The refresh is a trigger, not a gate: a
      backend that cannot be reached stays declared and shows as down
- [x] 15.9a Replace the empty-set rejection at `internal/config/config.go:110`, which currently
      fails `Load` when a file declares no backends, with a presence check. Removing the last
      backend otherwise commits an empty set that the next start cannot load, so a supported
      operation would leave a daemon that will not boot; no spec scenario depends on the set being
      non-empty. Do NOT simply delete the check: `Backends` is a plain map, so `{}`,
      `{"backends": null}` and a top-level `null` all unmarshal to a nil map and would then all be
      accepted, and a malformed hand edit would boot with every backend silently absent. Accept a
      present, non-null `backends` object including `{}`, and keep rejecting a missing or null one.
      Failing tests first, one per accepted and rejected shape, parameterized in one test
- [x] 15.10 Implement the remove operation in commit order: validate the name, confirm it is
      declared, commit the config write, drop the name from the declared-set snapshot under its
      write lock, then `Registry.Remove`, then the cleanup of 15.11. Never tear down before the
      write commits, or a refused write kills a live backend
- [x] 15.11 Implement removal cleanup, idempotent and non-aborting: evict the backend's tools,
      delete its override entry, delete its stored OAuth record. Each tolerates an absent target,
      and a failure in one is reported without skipping the others. A failure here is what 15.5a
      and 15.13 exist to survive
- [x] 15.12 Implement reload. Read and validate the entire file inside the writer mutex and
      return without touching anything if it does not load or any declaration is invalid. Adopt
      the validated bytes as the baseline and refresh the declared-set snapshot in the same
      critical section. Validating first and taking the mutex afterwards would let a concurrent
      hand edit make reload report success while applying an obsolete declaration, including
      starting a stdio child the current file no longer declares. Then release the writer mutex
      and diff against the registered set: unchanged backends are not touched at all; new ones go
      through `Add` with enabled state true; a name that has disappeared is a removal, including
      the declared-set drop and the cleanup of 15.11
- [x] 15.12a A changed declaration under an unchanged name captures the existing backend's
      enabled state, tears it down, and re-adds it with that captured state, deleting the stored
      OAuth record when the declaration identity changed. Passing enabled unconditionally would
      silently enable a disabled backend, and for a stdio backend that starts the process the
      user disabled. Hold the OUTGOING backend's transition lock continuously across the capture,
      the registry mutation and the teardown, and finish every persisted-state hand-off (15.12b)
      BEFORE publishing the replacement. The operation lock does not cover enable and disable, so a
      disable landing mid-replacement would be reported as succeeding while the replacement comes up
      enabled. Holding the outgoing lock alone is not enough either: a transition lock belongs to a
      backend object, not to a name, so once the replacement is published an enable takes the NEW
      object's lock and never sees the old one held. Publication last is what closes that. Failing
      tests first, one with a concurrent disable and one with a concurrent enable
- [x] 15.12b When such a replacement preserves a DISABLED state and the declaration identity
      changed, also re-persist the override entry under the new identity, so the recorded state
      matches the declaration it now belongs to. This is hygiene: 15.13's rebind-and-honour rule
      keeps the entry in force either way, which is what makes a crash partway through a
      replacement safe. Test across a simulated restart, because a runtime-only assertion passes
      while a whole class of bug in this area is present
- [x] 15.12c Trigger a catalog refresh for every backend reload adds or replaces. `Registry.Add`
      does not do it: 15.9 triggers the refresh separately for the add route, so reload must too.
      Without it a hand-added backend is registered but its tools never appear until the next TTL
      tick, and a replacement is left with no tools at all because its removal evicted them. The
      hand-added-appears-on-reload scenario asserts the tools are listed, so it pins this
- [x] 15.13 Implement the identity binding on both state stores: persist the declaration identity
      tuple (resource URL, auth mode, transport kind) on each OAuth record and each override
      entry, and compare it against the current declaration before honouring either. Comparing
      the resource URL alone is not sufficient, because an unchanged URL hides a change to either
      of the others. The two stores resolve a mismatch in OPPOSITE directions and that is
      deliberate, so it needs a comment at each site: a token is discarded, deleted and reported
      as needs-auth, while an override for a still-declared name is rebound to the current
      declaration and honoured. A mismatch cannot distinguish a stale entry from a repointed one
      without a generation counter, so each store fails toward its own safe answer, and for a
      disable that means not starting a process the user stopped
- [x] 15.13a Failing tests for the token side first: a record whose stored identity differs is
      never presented and the backend reports needs-auth; a matching record is used normally; a
      record written by an earlier version with no stored identity is treated as a mismatch
- [x] 15.13b Put the token comparison where the handler is obtained, not only where a record is
      read. `Store.Handler` at `internal/oauthstore/store.go:212` returns a cached handler per
      backend name and never rereads disk on a hit, and that handler owns a live token source
      holding the token in memory, so a check on the disk read alone never runs for a primed
      handler. Deleting a record must also discard that backend's cached handler, its token
      source and any pending authorization. Failing test first: prime a handler, delete the
      record, and confirm the predecessor's token is not presented
- [x] 15.13c Failing tests for the override side, both of which fail today: an entry recording a
      different identity for a still-declared backend keeps it disabled across a restart; an
      override file written before this change, carrying names and no identities at all
      (`overrideDocument` is `{"disabled": ["name"]}`), still disables every backend it lists on
      the first start after the upgrade. The second is the migration case, and getting it wrong
      silently starts every disabled stdio child on upgrade
- [x] 15.14 Implement startup reconciliation: delete any override entry and any stored OAuth
      record whose backend is not declared. For state whose backend IS declared but whose identity
      differs, follow 15.13's split rather than deleting both alike: delete the OAuth record,
      rebind and honour the override. Deleting a mismatched override here would undo 15.13c at the
      one moment it matters most, the first start after the upgrade, when no entry carries an
      identity. Test that a declared backend's own matching state survives, and that a mismatched
      pair resolves in the two different directions
- [x] 15.15 Add three routes to `routes()`, which is the sole registration path: `POST
      /api/backends`, `POST /api/backends/{name}/remove`, `POST /api/reload`. All three
      `mutates: true`, none nonce-guarded, so they inherit the origin, method and loopback-host
      guards. Reject an invalid name or declaration with 400, a duplicate name with 409, and a
      stale file with 409 carrying text the page can show. Surface a post-commit warning as a
      success with a note, not as a failure
- [x] 15.16 Extend the page and the assets: an add form covering both transports, and a remove
      action using the existing `data-confirm` second-confirming-action mechanism. Insert every
      value with `textContent`. The add form MUST NOT prefill or display any existing `env` or
      `headers` value, and the status snapshot must continue to omit both, because a declaration
      can carry an inline credential
- [x] 15.17 Mutation-verify, at minimum: drop the staleness comparison and the refusal test must
      fail; drop the baseline advance and the second-write test must fail; drop reload's baseline
      adoption and the write-after-reload test must fail; move reload's validation outside the
      writer mutex and the edited-between test must fail; replace the atomic exchange with a
      copy-then-rename and the archive-recovery test must fail; reinstate a plain-rename fallback
      and the unsupported-exchange test must fail; move the displaced-inode restriction to before
      the exchange and 15.3da's editor-swap test must fail; write through the unresolved path and
      15.3db's symlink test must fail; skip reload's latch check and 15.5c's shutdown-then-reload
      test must fail; publish the replacement before the state hand-off completes and 15.12a's
      concurrent-enable test must fail; check the shutdown latch inside
      `Registry.Add` instead of before the commit and the add-during-shutdown test must show a
      committed declaration with no registry entry; drop the transition hold in 15.12a and the
      concurrent-disable test must fail; delete a mismatched override at startup and the
      upgrade test must fail; accept `{"backends": null}` and 15.9a's rejected-shape test must
      fail; make an archive failure fatal and the
      commit-with-warning test must fail; apply a flat retention cap to every archive and the
      survives-ten-writes test must fail; drop the raw-message preservation and the
      unmodelled-field test must fail; loosen the name validator and the traversal test must fail
      on both routes; move the hygiene deletion before the exchange and the
      rejected-unavailable-exchange test must fail; move it after `Registry.Add` and the
      add-does-not-authenticate test must fail; make `Add` consult the override store and the
      starts-enabled test must fail; make reload's replacement pass enabled unconditionally and
      the stays-disabled test must fail; reinstate the empty-set rejection in `config.Load` and
      the remove-the-last-backend test must fail; make a mismatched override discard rather than
      rebind and both 15.13c tests must fail; skip the handler-cache invalidation on record
      deletion and 15.13b's primed-handler test must fail; drop reload's catalog refresh and the
      hand-added-appears-on-reload test must fail; leave `Shutdown` unlocked and unlatched and
      15.5b's race test must fail under `-race`; release the declared-set read lock between the
      check and
      the write and the override-racing-removal test must fail; drop the declared check in
      `Overrides.set` and that same test must fail; drop it in the token source and the
      refresh-after-removal test must fail; compare only the resource URL instead of the identity
      tuple and the changed-auth-mode test must fail; treat an absent identity as a match and the
      legacy-record test must fail; skip the override identity comparison and the
      enabled-after-restart test must fail; move the teardown before the config write and the
      refused-write test must fail; release the operation lock at the commit point and the
      concurrency test must fail; make reload apply a partially invalid file and the
      all-or-nothing test must fail; make reload rebuild every backend and the unchanged-backend
      test must fail; skip the `forShutdown` latch on remove and the child-exits test must fail
- [x] 15.18 Tests green under `-race`, then commit
