## Architecture

One long-lived `systemd --user` daemon, replacing a spawn-per-client model in which four
clients each spawned their own proxy, which each spawned their own copy of every stdio
backend.

```
                    +------------------------------ mcpd (systemd --user) ---+
claude code --+     |                                                        |
codex       --+---> |  /mcp/passthrough  -+                                  |
              |     |                     +-> session manager --> backends   |
cursor cli  --+---> |  /mcp/search       -+        |                         |
opencode    --+     |                              +- catalog + embeddings   |
zed (acp)   --+     |  /            web UI  -------+                         |
                    |  /oauth/callback      -------+- token store            |
                    +--------------------------------------------------------+
                       127.0.0.1:7420 (loopback only)
```

Everything is served from one `http.ServeMux`: two `StreamableHTTPHandler`s and the web
routes, sharing one cross-origin protection value.

### Package layout

| package | responsibility |
|---|---|
| `cmd/mcpd` | flags, wiring, `serve` and `install` subcommands, graceful shutdown |
| `cmd/oauthprobe` | provider feasibility probe (see Phase 0) |
| `cmd/evalrank` | ranking eval runner, deliberately not a `go test` |
| `internal/config` | config loading, `${VAR}` expansion, `cmd.Env` construction |
| `internal/backend` | session lifecycle, dispatch gate, registry, overrides |
| `internal/catalog` | canonical ids, persistence, coalescing refresh |
| `internal/embedding` | embeddings client, batching, content-hash cache |
| `internal/rank` | lexical scorer, fusion, abstention and calibration |
| `internal/oauthstore` | token persistence, pending-auth registry, code fetcher |
| `internal/mcpsrv` | the two MCP servers |
| `internal/web` | guard, status API, status page, inspector, OAuth routes |
| `internal/install` | client rewiring, approval migration, surgical revert |
| `internal/testfake` | in-process fake backend shared by tests |

## Key decisions

### Go rather than growing the Python prototype

The prototype's apparent asset was a ranker with a measured eval. That argument did not
survive the requirements: fusion replaces pure lexical scoring, embeddings are added, an
abstention signal is added, and the eval changes format, failure semantics, and size. The
only code that survives is a roughly 50-line lexical scorer, and the eval queries, which
are data.

What actually decided it is that the new code is overwhelmingly process supervision and
concurrency:

- The dispatch gate is `sync.RWMutex`. A lease is `RLock`; close-and-drain is `Lock`.
- The lifecycle lock, cancel-and-await, and the refresh loop are `context` and channels.
- `CommandTransport{Command *exec.Cmd}` makes `cmd.Env` explicit, so the prototype's
  partially-specified-environment bug is unwritable rather than merely avoidable.
- Cross-origin protection is on by default for localhost, rather than off unless
  configured.
- Deployment is a static binary with the UI embedded, matching the other Go tools already
  on this machine.

### Two endpoints rather than one mode parameter

Paths, not a query parameter, because some clients normalise or drop query strings on MCP
URLs. Mode is per client because stacking the facade behind a client that already has
native tool search ranks worse than either layer alone and saves nothing.

That last claim is measured, not assumed. Against the Python prototype over a 583-tool
catalog, a client with native tool search consumed 40,071 tokens through the facade versus
40,129 tokens natively: a 58-token difference, or 0.14%. The facade's entire value
proposition is token reduction, and against a client that already searches it delivers
none, while adding a layer that must guess a query to hand to the proxy's search.

### Ranking baseline to beat

The prototype's lexical-only ranker scored top-1 11/15 and top-3 15/15 over that same
583-tool catalog. Those 15 queries are carried forward verbatim as a regression baseline
rather than reworded, so the fusion work has a fixed bar it must not regress. The eval's
acceptance gate (80% top-1, 95% top-3) is set over the expanded set, not these 15.

### Reciprocal rank fusion, and abstention from raw evidence

Fusion avoids normalising a lexical score against a cosine similarity, which is the usual
source of silent mis-weighting, and degrades cleanly when one ranker is absent.

Abstention deliberately does not read the fused score. Reciprocal rank fusion encodes rank
position and ranker agreement, so the top result of any query scores about the same whether
it is a perfect match or the least-bad of six hundred irrelevant tools. Confidence is
therefore computed from the best raw cosine similarity, since cosine similarity is on an
absolute scale comparable across queries, which is exactly the property fusion discards.

Cosine is the only thresholded signal. An earlier draft also thresholded the raw lexical
score, but the ported scorer is an unnormalised idf sum whose magnitude tracks matched-term
count and term rarity, so its scores are not comparable across queries. Applying the rule
below to a mixed-length query set finds no separating gap under any of five normalisation
variants (raw, and divided by total idf, matchable idf, query length, and matched-term
count), so the rule disables the lexical signal in every variant and there is nothing for an
AND to bind. Cosine-only is also self-correcting: any weak-cosine answerable query drags the
floor down and erases the gap, so the rule cannot mint a threshold that falsely flags. The
lexical bound is still computed by the same rule and recorded, but nothing reads it.

Thresholds are calibrated against three disjoint sets: answerable queries establish the
floor true answers clear, a separate negative calibration set establishes the ceiling
irrelevant matches stay under, and a small negative validation set is scored once and never
used to choose anything. If the bounds do not separate there is no gap, which is a finding
about the embedding model rather than a number to adjust, and abstention ships disabled.

### At-most-once, with the boundary at "was a send attempted"

An earlier formulation drew the line at "before the request is written", which reads as
precise but is not observable from any write API: a write can raise after the bytes were
delivered and acknowledged. "Was a send attempted" is observable, being a property of the
session checked before touching the transport, and it errs in the safe direction. The cost
of being wrong is a spurious error the caller may retry deliberately, rather than a pull
request opened twice.

### Coalescing refresh, not single-flight join

A trigger arriving during an in-flight read must not be satisfied by that read, because a
tool-list-changed notification means "your last read is stale" and the in-flight read
started before the change. Joining it answers the wrong question and leaves the catalog
wrong until TTL expiry.

A trigger counter plus debounce and backoff gives "a read strictly after the most recent
trigger" without an unbounded call rate. There is deliberately no commit-sequence number:
reads of one backend are serialized, so commits are already ordered, and the check would be
unreachable code. Three counters are easy to confuse, so: the trigger counter decides
whether to read again, the generation counter catches a lifecycle transition invalidating
an in-flight read, and a commit sequence is not needed.

### Reusing the SDK's OAuth client and origin protection

The SDK provides authorization-code flow with PKCE, dynamic client registration,
pre-registered-client and client-metadata modes, protected-resource metadata discovery
including challenge parsing, and refresh via `oauth2.TokenSource`. Downstream OAuth is
therefore a persistence implementation plus a code fetcher and a callback route.

Origin protection likewise comes from the SDK and the standard library. One
`http.CrossOriginProtection` value is shared with the web routes so policy cannot diverge
between the two surfaces.

## Rejected alternatives

**Adopt an existing MCP gateway.** ContextForge offers proven OAuth and an admin UI, but
it is a multi-tenant enterprise gateway with role-based access control, teams, and
migrations, and this is one user on loopback. Critically, no surveyed gateway ships a tool
search facade, so the single most important behaviour would have had to be built inside
an extension point whose suitability was unverified. Trading a known quantity for an
unknown one on the load-bearing feature.

**MetaMCP.** Tool search is on its roadmap rather than in it, and its OAuth is inbound
only, so both required capabilities would still need building.

**1MCP.** Real downstream OAuth and light to run, but its progressive discovery is a CLI
the agent shells out to rather than MCP tools, and it has no status surface. Both halves
would need adding inside a codebase not under our control.

**Blending ranker scores instead of fusing ranks.** Requires normalising incommensurable
scales, and gets silently mis-weighted rather than visibly broken.

**Server-validated CSRF tokens.** With a host allowlist, origin validation, JSON-only POST
mutations, and escaped output, a token would defend only against a same-origin attacker,
which the escaping requirement is what actually addresses. Token plumbing would be process
rather than protection.

**Whole-file snapshot restore for revert.** Silently destroys unrelated user edits made
after installation, and the backup file makes that look like the safe behaviour.

**OS-level sandboxing of stdio backends.** Correct eventually, and out of scope here. It is
a distinct subsystem with its own failure modes, and the exposure it addresses predates
this daemon and is unchanged by it: the same third-party packages already run as this user
under the existing clients.

## Accepted risks

- **Single point of failure.** Four independent partial failures become one total failure.
  `Restart=always` covers crashes; it does not cover a bad configuration.
- **A hostile stdio backend is not contained.** It shares the user's UID and can read the
  token store directly, so the curated environment addresses accidental grants and
  auditability, not containment.
- **Workspace roots are daemon-global.** One shared child process means one roots value for
  every client and project.
- **Embeddings require the gateway at refresh time.** A warm cache is fully functional
  offline; only newly appeared tools degrade to lexical ranking.
- **A backend advertising a long list-cache TTL** would serve a manual re-index from cache,
  and the SDK exposes no way to invalidate. The TTL is surfaced in status so the cause is
  visible, and reconnect forces freshness.

## Phase 0 finding

Provider feasibility was verified before implementation depended on it. Discovery,
dynamic client registration, and a plain-HTTP loopback redirect URI all work against the
real provider, which registered as a public client with no secret. Recorded in
`PHASE0.md`.

The probe also invalidated three assumed SDK signatures, which is the concrete argument for
confirming signatures with `go doc` before writing code against them.
