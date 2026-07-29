package rank

import (
	"math"
	"sort"

	"github.com/ahodges/mcpd/internal/catalog"
)

// rrfK is the reciprocal rank fusion constant: each ranker contributes
// 1/(rrfK+rank) for an entry it ranked.
const rrfK = 60

// Evidence is the raw, per-query best score each ranker produced, before
// fusion collapses everything to rank position. Abstention reads these
// instead of the fused score, because reciprocal rank fusion makes the top
// result of any query score about the same regardless of whether it is a
// perfect match or the least-bad of many irrelevant tools.
// HasCosine distinguishes "no entry was scored semantically" from a genuine
// cosine of zero, which abstention needs: without it, the lexical-only
// degraded mode reads as every query being maximally unlike every tool.
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
// An entry missing from vecs, or a nil qvec, contributes no semantic term
// and ranks on its lexical score alone, which is how ranking degrades
// cleanly with no embeddings at all, or with a partially warm cache after
// new tools appear.
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

	type semScore struct {
		id    string
		score float64
	}
	var semantic []semScore
	if qvec != nil {
		for _, e := range entries {
			v, ok := vecs[e.ID]
			if !ok {
				continue
			}
			semantic = append(semantic, semScore{e.ID, cosine(qvec, v)})
		}
		sort.SliceStable(semantic, func(i, j int) bool {
			if semantic[i].score != semantic[j].score {
				return semantic[i].score > semantic[j].score
			}
			return semantic[i].id < semantic[j].id
		})
	}
	semRank := make(map[string]int, len(semantic))
	var bestCosine float64
	for i, s := range semantic {
		semRank[s.id] = i + 1
		if i == 0 {
			bestCosine = s.score
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

	fused := make([]Result, 0, len(seen))
	for id := range seen {
		var score float64
		if r, ok := lexRank[id]; ok {
			score += 1.0 / float64(rrfK+r)
		}
		if r, ok := semRank[id]; ok {
			score += 1.0 / float64(rrfK+r)
		}
		e := byID[id]
		fused = append(fused, Result{ID: e.ID, Server: e.Server, Description: e.Description, Score: score})
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		return fused[i].ID < fused[j].ID
	})
	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}
	return fused, Evidence{BestCosine: bestCosine, BestLexical: bestLexical, HasCosine: len(semantic) > 0}
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
