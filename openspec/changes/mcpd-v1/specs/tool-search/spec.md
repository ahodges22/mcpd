## ADDED Requirements

### Requirement: Three-tool search facade

The search facade SHALL expose exactly three tools: `search_tools`, `describe_tool`, and
`call_tool`. `describe_tool` SHALL return a tool's full input schema from the catalog
without contacting the upstream.

#### Scenario: The facade advertises three tools

- **WHEN** a client lists tools on the search endpoint
- **THEN** exactly three tools are returned regardless of how many backends are connected

#### Scenario: Schema lookup costs no upstream call

- **WHEN** `describe_tool` is called for a known id
- **THEN** the schema is served from the catalog and no request reaches the backend

#### Scenario: An empty catalog is explained, not silently empty

- **WHEN** `search_tools` runs against an empty catalog
- **THEN** the result explains why no tools are available rather than returning a bare
  empty list

### Requirement: Reciprocal rank fusion for ordering

Ranking SHALL fuse a lexical ordering and an embedding-similarity ordering by reciprocal
rank, summing `1 / (60 + rank)` across rankers. Raw scores SHALL NOT be blended, because
a lexical score and a cosine similarity are not commensurable and normalising one against
the other silently mis-weights results.

#### Scenario: Ranks are fused rather than scores

- **WHEN** a tool ranks first lexically and third semantically
- **THEN** its fused score is the sum of the two reciprocal-rank terms

#### Scenario: Ranking degrades cleanly without embeddings

- **WHEN** no embedding vectors are available
- **THEN** ranking proceeds on the lexical ordering alone without erroring

### Requirement: Embeddings fail soft and are cached by content

Tool embeddings SHALL be requested from the configured gateway at catalog-refresh time
and cached per tool, keyed by a hash of the tool's name, description, and schema, so a
tool whose description has not changed is never re-embedded. An unreachable embeddings
service SHALL degrade ranking to lexical-only rather than failing the request.

#### Scenario: The gateway is unreachable

- **WHEN** the embeddings service cannot be reached during a refresh
- **THEN** ranking continues lexical-only and the status surface reports how many tools
  are unvectorized

#### Scenario: A warm cache works offline

- **WHEN** the daemon runs with no network access and a previously populated cache
- **THEN** every already-embedded tool ranks with full fidelity, and only newly appeared
  tools fall back to lexical

### Requirement: Abstention is computed from raw evidence

The facade SHALL signal low confidence from the best raw cosine similarity and the best
raw lexical score, and SHALL NOT derive confidence from the fused score. The fused score
encodes rank position and ranker agreement rather than relevance: the top result of a
hopeless query scores about the same as the top result of an easy one, so no threshold on
it can distinguish the two.

#### Scenario: A query nothing serves is flagged

- **WHEN** a query has no good match in the catalog and both raw signals fall below their
  thresholds
- **THEN** candidates are still returned but flagged low-confidence rather than presented
  as answers

#### Scenario: Abstention ships disabled rather than miscalibrated

- **WHEN** threshold calibration finds no separating gap between answerable and
  no-answer evidence
- **THEN** abstention is disabled and the absent gap is recorded, because a confidence
  flag that is noise is worse than none

### Requirement: Ranking accuracy is gated by an eval

Ranking changes SHALL be gated by an eval over a fixed query set: at least 80% top-1 and
95% top-3 across the answerable queries. The eval SHALL fail rather than score when any
expected tool is absent from the catalog, because scoring a shrunken denominator inflates
the result and reads as a pass.

#### Scenario: An incomplete catalog fails the run

- **WHEN** the eval runs against a catalog missing a backend whose tools it expects
- **THEN** the run exits non-zero rather than skipping those queries and reporting a
  percentage over the remainder

#### Scenario: A query may have several correct answers

- **WHEN** a query is legitimately served by more than one tool
- **THEN** a top-1 hit is counted if the first result is any member of the acceptable set
