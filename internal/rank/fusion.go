package rank

import (
	"math"
	"sort"

	"github.com/ahodges/mcpd/internal/catalog"
)

// rrfK is the reciprocal rank fusion constant: each ranker contributes
// 1/(rrfK+rank) for an entry it ranked.
const rrfK = 60

// fuseDepth is how far down each ranking a rank still counts.
//
// Reciprocal rank fusion assumes it is given comparable top-k lists. These two are not: the
// lexical ranker returns only documents that share a term, often a handful, while the cosine
// ranker scores every tool in the catalog. Uncapped, being first of five lexical matches is
// worth exactly as much as being the single best semantic match out of 583, and a tool that
// shares no vocabulary with the query cannot reach the top three however well it matches its
// meaning: measured over the eval set, "combine these branches once review passes" ranked
// merge_pull_request first by cosine and did not return it at all.
//
// Capping both lists at one depth restores the precondition. 50 is the depth at which the
// eval stops improving, and it is well beyond the three results the facade returns.
const fuseDepth = 50

// Evidence is the raw, per-query best score each ranker produced, before
// fusion collapses everything to rank position. Abstention reads these
// instead of the fused score, because reciprocal rank fusion makes the top
// result of any query score about the same regardless of whether it is a
// perfect match or the least-bad of many irrelevant tools.
// HasCosine is true only when at least one genuinely comparable vector was
// scored, which is stronger than "a comparison was attempted": it
// distinguishes an absent reading from a genuine cosine of zero. Abstention
// needs that distinction, because without it the lexical-only degraded mode,
// and any input that makes every comparison degenerate, read as every query
// being maximally unlike every tool.
type Evidence struct {
	BestCosine  float64
	BestLexical float64
	HasCosine   bool
}

// Fuse orders entries by summing 1/(rrfK+rank) across a lexical ranking and
// a cosine-similarity ranking, never by blending the two raw scores: a
// lexical weight and a cosine similarity are not on a common scale, and
// normalising one against the other is a silent source of mis-weighting.
//
// vecs holds a cached vector per entry ID; qvec is the query's own vector.
// An entry missing from vecs, or one whose vector cosine cannot compare
// against qvec, contributes no semantic term and ranks on its lexical score
// alone, which is how ranking degrades cleanly with no embeddings at all,
// with a partially warm cache after new tools appear, and with a cache that
// outlived a change to the embedding model's dimension.
func Fuse(query string, entries []catalog.Entry, vecs map[string][]float32, qvec []float32, limit int) ([]Result, Evidence) {
	lexical := Lexical(query, entries)
	lexRank := make(map[string]int, len(lexical))
	var bestLexical float64
	for i, r := range lexical {
		lexRank[r.ID] = i + 1
		if i == 0 {
			bestLexical = r.Score
		}
	}

	semantic := Cosine(entries, vecs, qvec)
	semRank := make(map[string]int, len(semantic))
	var bestCosine float64
	for i, s := range semantic {
		semRank[s.ID] = i + 1
		if i == 0 {
			bestCosine = s.Score
		}
	}

	byID := make(map[string]catalog.Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	seen := make(map[string]bool, len(lexRank)+len(semRank))
	for id := range lexRank {
		seen[id] = true
	}
	for id := range semRank {
		seen[id] = true
	}

	sim := make(map[string]float64, len(semantic))
	for _, s := range semantic {
		sim[s.ID] = s.Score
	}

	fused := make([]Result, 0, len(seen))
	for id := range seen {
		lex, inLex := within(lexRank, id, min(len(lexRank), fuseDepth))
		sem, inSem := within(semRank, id, min(len(semRank), fuseDepth))
		if !inLex && !inSem {
			// Outside both lists' depth, so neither ranker is making a claim about it.
			continue
		}
		score := 1.0/float64(rrfK+lex) + 1.0/float64(rrfK+sem)
		e := byID[id]
		fused = append(fused, Result{ID: e.ID, Server: e.Server, Description: e.Description, Score: score})
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		// Capping makes exact ties common, because appearing at the same rank in one list is
		// now the usual case. Broken on raw cosine similarity, which unlike the fused score
		// is on an absolute scale comparable across queries: that is the same property
		// abstention relies on, and the reason it reads cosine rather than the fused score.
		if sim[fused[i].ID] != sim[fused[j].ID] {
			return sim[fused[i].ID] > sim[fused[j].ID]
		}
		return fused[i].ID < fused[j].ID
	})
	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}
	return fused, Evidence{BestCosine: bestCosine, BestLexical: bestLexical, HasCosine: len(semantic) > 0}
}

// Cosine ranks entries by the similarity of their cached vector to the query's, highest
// first. An entry with no cached vector, or one whose vector cannot be compared against the
// query's, is absent rather than scored zero: a zero is indistinguishable from a genuine
// orthogonal reading, and treating a missing vector as maximally dissimilar would rank a tool
// below every tool that was merely irrelevant.
//
// Exported so the eval can report where a tool sat in each ranker separately. Fusion can only
// promote what one of its inputs already ranked, so which input failed is the first thing to
// establish about a miss.
func Cosine(entries []catalog.Entry, vecs map[string][]float32, qvec []float32) []Result {
	if len(qvec) == 0 {
		return nil
	}
	out := make([]Result, 0, len(entries))
	for _, e := range entries {
		v, cached := vecs[e.ID]
		if !cached {
			continue
		}
		sim, ok := cosine(qvec, v)
		if !ok {
			continue
		}
		out = append(out, Result{ID: e.ID, Server: e.Server, Description: e.Description, Score: sim})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// within reports an entry's rank when a ranker placed it inside the fusion depth.
//
// An entry the ranker did not place is given the rank just past the end of that ranker's own
// list, because absence from the lexical list means the entry sits below every document that
// shared a term with the query: a weak opinion rather than no opinion. Scoring it as nothing
// is what made a single incidental term match outrank the best semantic match in the catalog,
// since any entry in both lists then beat any entry that was first in one, however good. The
// floor is per list rather than a constant: the lexical list is usually a few dozen documents
// long, and charging an absence the same as rank 51 there would over-penalise it just as
// badly in the other direction.
func within(ranks map[string]int, id string, depth int) (rank int, placed bool) {
	if r, ok := ranks[id]; ok && r <= fuseDepth {
		return r, true
	}
	return depth + 1, false
}

// cosine reports the similarity of two vectors, and whether they were
// comparable at all. Mismatched widths and zero-magnitude vectors have no
// defined similarity, and a 0 returned for them is indistinguishable from a
// genuine orthogonal reading, which abstention would read as real evidence
// that nothing matches.
func cosine(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), true
}
