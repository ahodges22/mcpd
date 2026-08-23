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

### Requirement: Candidate fusion and bounded reranking for ordering

The fallback ranking SHALL fuse a lexical ordering and an embedding-similarity ordering by reciprocal
rank, summing `1 / (60 + rank)` across rankers. Raw scores SHALL NOT be blended, because
a lexical score and a cosine similarity are not commensurable and normalising one against
the other silently mis-weights results.

The primary ranking SHALL build a candidate union from the lexical, base-cosine, and
generated-query-centroid top lists, then ask the configured chat reranker for an ordering in one
bounded attempt. Only candidate IDs SHALL be accepted from that output. A unique exact tool-name
match of at least two tokens SHALL take precedence, the existing backend-diversity cap SHALL apply
after reranking, and any rerank failure SHALL return the reciprocal-rank fallback. Tool names and
descriptions SHALL be framed as untrusted data. This reduces but does not eliminate the risk that
a malicious description manipulates relevance ordering.

#### Scenario: Ranks are fused rather than scores

- **WHEN** a tool ranks first lexically and third semantically
- **THEN** its fused score is the sum of the two reciprocal-rank terms

#### Scenario: Ranking degrades cleanly without embeddings

- **WHEN** no embedding vectors are available
- **THEN** ranking proceeds on the lexical ordering alone without erroring

#### Scenario: A semantic candidate can win without lexical overlap

- **WHEN** the expected tool is retrieved by base or expanded cosine but shares no query term
- **THEN** it remains eligible for the final top three rather than receiving an absence penalty

#### Scenario: Reranking fails within its deadline

- **WHEN** the reranker times out, errors, or returns no allowlisted candidate ID
- **THEN** search returns the reciprocal-rank fallback with the same backend-diversity cap

### Requirement: Embeddings fail soft and are cached by content

Tool embeddings SHALL be requested from the configured gateway at catalog-refresh time
and cached per tool, keyed by a hash of the exact text sent for embedding, so a tool whose
embedded text has not changed is never re-embedded. An unreachable embeddings
service SHALL degrade ranking to lexical-only rather than failing the request.

#### Scenario: The gateway is unreachable

- **WHEN** the embeddings service cannot be reached during a refresh
- **THEN** ranking continues lexical-only and the status surface reports how many tools
  are unvectorized

#### Scenario: A warm cache works offline

- **WHEN** the daemon runs with no network access and a previously populated cache
- **THEN** every already-embedded tool ranks with full fidelity, and only newly appeared
  tools fall back to lexical

Generated query expansions SHALL be cached with their text and centroid. The cache header SHALL
record the generator model, generation-prompt hash, embedding model, and vector dimension. Catalog
refreshes SHALL coalesce to the newest snapshot so simultaneous backend commits neither duplicate
generation calls nor publish an older index after a newer one.

### Requirement: Abstention is computed from raw evidence

The facade SHALL signal low confidence from the best raw cosine similarity, and SHALL NOT
derive confidence from the fused score.

Cosine is the only thresholded signal. The lexical scorer is an unnormalised idf sum whose
magnitude tracks matched-term count and term rarity, so its scores are not comparable
across queries: applying this requirement's own calibration rule to a mixed-length query
set finds no separating gap under any of five normalisation variants (raw, and divided by
total idf, matchable idf, query length, and matched-term count). A signal the rule disables
in every variant cannot be part of an AND. Cosine similarity is on an absolute scale and is
comparable across queries, which is the property this requirement depends on. The lexical
bound is still computed by the same rule and recorded, but nothing reads it. The fused score
encodes rank position and ranker agreement rather than relevance: the top result of a
hopeless query scores about the same as the top result of an easy one, so no threshold on
it can distinguish the two.

#### Scenario: A query nothing serves is flagged

- **WHEN** a query has no good match in the catalog and the best raw cosine similarity
  falls below its threshold
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
