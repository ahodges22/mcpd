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

- [ ] 11.1 Write the failing test: all three surfaces answer, and the two MCP endpoints
      advertise different tool counts
- [ ] 11.2 Implement `cmd/mcpd serve`: one mux, both MCP handlers wrapped in
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
- [ ] 11.2a Immediately after `catalog.Load()`, call `catalog.Drop(name)` for every backend
      that loaded disabled, and pin it with a test. A crash between Task 5's override write
      and its tool eviction leaves that backend's tools in the persisted catalog, and a
      disabled backend is never re-listed, so this is the only thing that ever removes
      them. It is a step of its own because a paragraph is skippable and this is not
- [ ] 11.3 Wire embeddings into ranking. This is a multi-package code change, not wiring:
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
- [ ] 11.4 Wire `mcpsrv.Passthrough.Sync` to the catalog's post-commit hook,
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
- [ ] 11.5 Wire all four catalog refresh triggers: `catalog.Start(ctx)` covers TTL expiry, and
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
- [ ] 11.6 Write the systemd user unit, relying on the existing user environment import
      rather than an environment file
- [ ] 11.7 Build, install, enable, and confirm the status endpoint answers with backends up.
      A backend that is down here is a missing passthrough variable, to be added rather than
      worked around by inheriting everything
- [ ] 11.8 Commit

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

- [ ] 15.1 Add one shared backend-name validator and apply it on both paths. The name
      becomes a URL path segment, the `oauth-<name>.json` file name under the state
      directory, and the prefix of every `mcp__<server>__<tool>` id, so it has three
      injection surfaces and until now was trusted because only a hand-edited file could
      supply it. Use `^[a-z0-9][a-z0-9_-]{0,63}$`, call it from `config.Load` as well as the
      add route, and confirm all 13 currently declared names still load
- [ ] 15.2 Write the failing tests for the write path, in `internal/config`: a field the
      daemon's types do not model survives an add; the replaced content is readable from the
      backup; a file changed on disk since load refuses the write and leaves the file byte
      identical; a mode of 0644 becomes 0600 after a write
- [ ] 15.3 Implement the write path. Read-modify-write over a preserving representation:
      decode the document and its `backends` object as `map[string]json.RawMessage` and
      re-encode only the entry that changed, so no unmodelled field is dropped. Record the
      bytes read at load, re-read and compare immediately before writing, and return a
      distinct sentinel error on mismatch so the route can answer 409. Copy the replaced
      content to `config.json.bak` in the same directory, then temp-file-plus-rename as
      `Overrides.save` already does. Stat the existing file and tighten the mode when it is
      readable beyond its owner, never loosen it. Marshalling a Go map sorts its keys, so the
      first write reformats the file into sorted order; that is accepted, and it is the reason
      the backup exists
- [ ] 15.4 Write the failing tests for runtime registration: an added backend serves a tool
      with no restart; a removed backend's session closes, its stdio child exits and its
      tools leave the catalog; a reload adopts a hand-added declaration and tears down a
      hand-removed one; a reload that changes one backend leaves every other backend's
      session, child and authorization intact
- [ ] 15.5 Implement `Registry.Add` and `Registry.Remove`. `backends` and `names` are
      currently immutable after `NewRegistry` and are read with no lock on the dispatch hot
      path, so add an RWMutex and take it for reading in `Get`, `Names` and `Health`. It sits
      **above** the existing four-level order (transition, gate, life, mu). Never hold it
      across a teardown: `Remove` takes it, deletes the entry, releases it, and only then
      tears the backend down, because teardown blocks on in-flight work. Tear down with the
      terminal `forShutdown` latch, so nothing can respawn a child for a backend that is no
      longer declared. `NewRegistry` must retain the `Hooks` value it was given, because
      `Add` needs it to build the new backend
- [ ] 15.6 Implement reload. Diff the freshly loaded declarations against the registered set:
      unchanged backends are not touched at all, new ones go through `Add`, absent ones
      through `Remove`, and a changed declaration is a `Remove` followed by an `Add`. A
      reload-driven replace MUST NOT delete the backend's override or its stored OAuth
      record, because the user changed a declaration rather than removing a backend; only an
      explicit removal in 15.7 deletes runtime state
- [ ] 15.7 Implement removal cleanup: delete the removed backend's override entry and its
      stored OAuth record. State left under a name that is no longer declared would silently
      apply to a later backend that reused the name
- [ ] 15.8 Add the three routes to `routes()`, which is the sole registration path:
      `POST /api/backends`, `POST /api/backends/{name}/remove`, and `POST /api/reload`. All
      three are `mutates: true` and none is nonce-guarded, so they inherit the origin, method
      and loopback-host guards. Reject a duplicate name with 409, an invalid declaration with
      400, and a stale file with 409 carrying text the page can show
- [ ] 15.9 Extend the page and the assets: an add form covering both transports, and a remove
      action requiring a second confirming action through the existing `data-confirm`
      mechanism. Insert every value with `textContent`. The add form MUST NOT prefill or
      display any existing `env` or `headers` value, and the status snapshot must continue to
      omit both, because a declaration can carry an inline credential
- [ ] 15.10 Mutation-verify at minimum: drop the staleness comparison and the refusal test
      must fail; drop the raw-message preservation and the unmodelled-field test must fail;
      loosen the name validator and the traversal test must fail; make reload rebuild every
      backend and the unchanged-backend test must fail; skip the `forShutdown` latch on remove
      and the child-exits test must fail
- [ ] 15.11 Tests green under `-race`, then commit
