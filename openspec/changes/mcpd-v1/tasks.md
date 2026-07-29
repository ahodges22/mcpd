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

- [ ] 6.1 Write the failing tests: fusion degrades to lexical with no vectors; ranks are
      fused rather than scores; an unreachable embeddings service is soft
- [ ] 6.2 Port the prototype's lexical scorer, including its tool-name squashing
- [ ] 6.3 Implement the embeddings client with batching and a content-hash cache
- [ ] 6.4 Implement reciprocal rank fusion
- [ ] 6.5 Tests green, then commit

## 7. Abstention signal

- [ ] 7.1 Write the failing tests: overlapping evidence yields disabled abstention and an
      error naming the overlap; separated evidence yields a threshold inside the gap
- [ ] 7.2 Implement the fixed selection rule against raw cosine and lexical evidence, never
      the fused score
- [ ] 7.3 Tests green, then commit

## 8. The two MCP endpoints

- [ ] 8.1 Write the failing tests: the facade advertises exactly three tools; pass-through
      advertises canonical names; a call reaches the owning backend; an empty catalog is
      explained rather than silently empty
- [ ] 8.2 Implement the facade server with `search_tools`, `describe_tool`, `call_tool`
- [ ] 8.3 Implement the pass-through server, syncing tools on catalog change so the SDK
      emits tool-list-changed
- [ ] 8.4 Tests green, then commit

## 9. Guard, status page, and inspector

- [ ] 9.1 Write the failing tests: a foreign origin is rejected on both an MCP endpoint and
      the status API; an absent origin is accepted; a mutation is rejected on GET; a tool
      result containing markup renders as literal text
- [ ] 9.2 Implement the guard: one shared cross-origin protection value, POST-only
      mutations, the callback exemption, and a deny handler carrying a reason
- [ ] 9.3 Implement the status API and the embedded page, inserting all dynamic values as
      text
- [ ] 9.4 Implement the inspector, rendering results as text and surfacing destructive
      annotations
- [ ] 9.5 Verify no markup-insertion API appears anywhere in the assets
- [ ] 9.6 Tests green, then commit

## 10. Downstream OAuth

- [ ] 10.1 Confirm the authorization-handler config field names with `go doc`
- [ ] 10.2 Write the failing tests: the full flow against a fake provider through
      registration, exchange, 0600 persistence, authenticated reconnect, restart reuse, and
      refresh; a mismatched or replayed state is rejected with no token written, driven as a
      browser-shaped GET through the real guard
- [ ] 10.3 Implement the store: pending-authorization registry, code fetcher, atomic
      persistence at 0600 inside a 0700 directory, and write-back on refresh
- [ ] 10.4 Wire the handler into the backend HTTP client using dynamic client registration,
      per the Phase 0 finding, and return a backend to needs-auth on refresh failure
- [ ] 10.5 Add the authorize and callback routes
- [ ] 10.6 Tests green under `-race`, then commit

## 11. Daemon entrypoint and first real run

- [ ] 11.1 Write the failing test: all three surfaces answer, and the two MCP endpoints
      advertise different tool counts
- [ ] 11.2 Implement `cmd/mcpd serve`: one mux, both MCP handlers sharing the guard's
      protection value, overrides loaded before connecting, graceful shutdown draining and
      terminating children. Must call `catalog.Load()` at startup (it is what backs the
      spec's persistence requirement) and one startup `catalog.RefreshAll()`, because
      `catalog.Start` deliberately performs no immediate refresh to avoid doubling every
      cold start's reads
- [ ] 11.3 Wire all four catalog refresh triggers: `catalog.Start(ctx)` covers TTL expiry, and
      `backend.Hooks{ToolListChanged, Reconnected}` both point at `Catalog.Trigger`. The
      mechanisms are built in Tasks 3 and 4; only the wiring is left, and an unwired hook is a
      silently stale catalog. Task 5 added three more hooks that must also be wired, or a
      disable stops being a kill switch: `StopRefresh` to `Catalog.StopRefresh`, `DropTools`
      to `Catalog.Drop`, and `Refresh` to `Catalog.Trigger`. `NewRegistry` now also takes the
      `*backend.Overrides` loaded from the state directory
- [ ] 11.4 Write the systemd user unit, relying on the existing user environment import
      rather than an environment file
- [ ] 11.5 Build, install, enable, and confirm the status endpoint answers with backends up.
      A backend that is down here is a missing passthrough variable, to be added rather than
      worked around by inheriting everything
- [ ] 11.6 Commit

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
      rather than scoring a shrunken denominator
- [ ] 13.5 Record the baseline, then calibrate thresholds and score the validation set
      exactly once
- [ ] 13.6 Iterate on fusion only if the gate fails, watching the held-out versus tuned gap
- [ ] 13.7 Commit

## 14. Live provider acceptance

- [ ] 14.1 Clear the stored token and confirm the backend reports needs-auth with a pending
      URL
- [ ] 14.2 Complete authorization in the browser and confirm the backend connects with a
      non-zero tool count
- [ ] 14.3 Make one real authenticated call through the inspector
- [ ] 14.4 Restart the daemon and confirm the token is reused with no re-authorization
- [ ] 14.5 Force access-token expiry and confirm a refresh rather than a return to needs-auth
- [ ] 14.6 Append the outcome to `PHASE0.md` and commit
