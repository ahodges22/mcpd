package rank

import (
	"math"
	"testing"

	"github.com/ahodges/mcpd/internal/catalog"
)

func TestFuseDegradesToLexicalWithoutVectors(t *testing.T) {
	entries := referenceCatalog()
	got, ev := Fuse("kubernetes pod logs", entries, nil, nil, 3)
	if len(got) == 0 || got[0].ID != "mcp__art__kubectl_logs" {
		t.Fatalf("lexical-only fusion failed: %+v", got)
	}
	if ev.BestCosine != 0 {
		t.Errorf("BestCosine = %v, want 0 with no vectors available", ev.BestCosine)
	}
	if ev.HasCosine {
		t.Error("HasCosine must be false with no vectors, so abstention can tell an absent reading from a zero one")
	}
}

// rrfEntries returns a catalog where "alpha" ranks 1st lexically and 3rd
// semantically, so its fused score is pinned to exactly the sum of the two
// reciprocal-rank terms. A score-blending implementation (normalising the
// raw lexical score against cosine similarity and summing or averaging
// those) would not land on this number, since alpha's raw lexical score
// (well above zero) and raw cosine (exactly zero) share no common scale.
func rrfEntries() ([]catalog.Entry, map[string][]float32, []float32) {
	entries := []catalog.Entry{
		{ID: "mcp__x__alpha_tool", Server: "x", Tool: "alpha_tool", Description: "alpha alpha alpha tool"},
		{ID: "mcp__x__beta_tool", Server: "x", Tool: "beta_tool", Description: "mentions alpha once"},
		{ID: "mcp__x__gamma_tool", Server: "x", Tool: "gamma_tool", Description: "no relevant text at all"},
	}
	vecs := map[string][]float32{
		"mcp__x__alpha_tool": {0, 1}, // cosine 0 against qvec: semantic rank 3
		"mcp__x__beta_tool":  {1, 0}, // cosine 1: semantic rank 1
		"mcp__x__gamma_tool": {0.7, 0.3},
	}
	qvec := []float32{1, 0}
	return entries, vecs, qvec
}

func TestFuseUsesReciprocalRankNotScoreBlending(t *testing.T) {
	entries, vecs, qvec := rrfEntries()

	lex := Lexical("alpha", entries)
	if len(lex) == 0 || lex[0].ID != "mcp__x__alpha_tool" {
		t.Fatalf("fixture invalid: alpha_tool must rank 1st lexically, got %+v", lex)
	}

	got, ev := Fuse("alpha", entries, vecs, qvec, 10)

	var alpha *Result
	for i := range got {
		if got[i].ID == "mcp__x__alpha_tool" {
			alpha = &got[i]
		}
	}
	if alpha == nil {
		t.Fatalf("alpha_tool missing from fused results: %+v", got)
	}

	want := 1.0/61.0 + 1.0/63.0
	if math.Abs(alpha.Score-want) > 1e-9 {
		t.Errorf("fused score = %v, want %v (1/(60+1) + 1/(60+3))", alpha.Score, want)
	}

	if math.Abs(ev.BestLexical-lex[0].Score) > 1e-9 {
		t.Errorf("Evidence.BestLexical = %v, want the raw top lexical score %v", ev.BestLexical, lex[0].Score)
	}
	if math.Abs(ev.BestCosine-1.0) > 1e-9 {
		t.Errorf("Evidence.BestCosine = %v, want 1.0 (beta_tool's exact cosine match)", ev.BestCosine)
	}
	if !ev.HasCosine {
		t.Error("Evidence.HasCosine = false, want true once any entry was scored semantically")
	}
}

func TestFuseFallsBackToLexicalOnlyForANewlyAppearedTool(t *testing.T) {
	entries := []catalog.Entry{
		{ID: "mcp__x__alpha_tool", Server: "x", Tool: "alpha_tool", Description: "alpha alpha alpha tool"},
		{ID: "mcp__x__new_tool", Server: "x", Tool: "new_tool", Description: "alpha newly added tool"},
	}
	// Only alpha_tool has a cached vector; new_tool just appeared and has none yet.
	vecs := map[string][]float32{"mcp__x__alpha_tool": {1, 0}}
	qvec := []float32{1, 0}

	got, ev := Fuse("alpha", entries, vecs, qvec, 10)

	ids := make(map[string]bool)
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids["mcp__x__new_tool"] {
		t.Errorf("a tool with no vector must still rank on its lexical score: %+v", got)
	}
	if ev.BestCosine != 1.0 {
		t.Errorf("BestCosine = %v, want 1.0 computed only from the vectorized tool", ev.BestCosine)
	}
}

// TestFuseReportsCosineEvidenceOnlyWhenSomethingWasComparable pins
// Evidence.HasCosine to "at least one genuinely comparable vector was scored"
// rather than "a comparison was attempted". Each degenerate input below makes
// cosine degenerate to 0 for every entry, which abstention would otherwise
// read as a real similarity of zero and use to flag every single query. The
// realistic route in is a warm cache outliving a change to the embedding
// model's dimension: cached vectors of the old width scored against a query
// vector of the new one.
func TestFuseReportsCosineEvidenceOnlyWhenSomethingWasComparable(t *testing.T) {
	entries := []catalog.Entry{
		{ID: "mcp__x__alpha_tool", Server: "x", Tool: "alpha_tool", Description: "alpha alpha alpha tool"},
		{ID: "mcp__x__beta_tool", Server: "x", Tool: "beta_tool", Description: "mentions alpha once"},
	}
	cases := []struct {
		name           string
		vecs           map[string][]float32
		qvec           []float32
		wantHasCosine  bool
		wantBestCosine float64
	}{
		{
			name: "every cached vector is a different width from the query vector",
			vecs: map[string][]float32{"mcp__x__alpha_tool": {1, 0, 0}, "mcp__x__beta_tool": {0, 1, 0}},
			qvec: []float32{1, 0},
		},
		{
			name: "the query vector is empty but not nil",
			vecs: map[string][]float32{"mcp__x__alpha_tool": {1, 0}, "mcp__x__beta_tool": {0, 1}},
			qvec: []float32{},
		},
		{
			name: "the query vector has zero magnitude",
			vecs: map[string][]float32{"mcp__x__alpha_tool": {1, 0}, "mcp__x__beta_tool": {0, 1}},
			qvec: []float32{0, 0},
		},
		{
			name:           "one comparable vector among degenerate ones still counts",
			vecs:           map[string][]float32{"mcp__x__alpha_tool": {1, 0}, "mcp__x__beta_tool": {0, 0}},
			qvec:           []float32{1, 0},
			wantHasCosine:  true,
			wantBestCosine: 1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ev := Fuse("alpha", entries, tc.vecs, tc.qvec, 10)

			if ev.HasCosine != tc.wantHasCosine {
				t.Errorf("HasCosine = %v, want %v", ev.HasCosine, tc.wantHasCosine)
			}
			if math.Abs(ev.BestCosine-tc.wantBestCosine) > 1e-9 {
				t.Errorf("BestCosine = %v, want %v", ev.BestCosine, tc.wantBestCosine)
			}
			if len(got) != len(entries) {
				t.Errorf("got %d results, want all %d: an incomparable vector must count as absent, not drop the tool", len(got), len(entries))
			}
			if !tc.wantHasCosine && (Thresholds{Cosine: 0.30, Enabled: true}).LowConfidence(ev) {
				t.Error("calibrated abstention flagged a query for which no cosine was ever computed")
			}
		})
	}
}

func TestFuseRespectsLimit(t *testing.T) {
	entries := referenceCatalog()
	got, _ := Fuse("get", entries, nil, nil, 1)
	if len(got) > 1 {
		t.Errorf("Fuse returned %d results, want at most 1", len(got))
	}
}

// The best semantic match in the catalog must be able to reach the results even when it
// shares no vocabulary with the query. Scoring an entry a ranker did not place as nothing
// rather than as a poor rank made presence in both lists worth more than being first in
// either, so any tool with one incidental term match outranked the top semantic hit: over the
// eval set, "combine these branches once review passes" ranked merge_pull_request first by
// cosine and did not return it at all.
func TestTheTopSemanticMatchOutranksAToolWithOnlyAnIncidentalTermMatch(t *testing.T) {
	entries := []catalog.Entry{
		// Shares no query term, and is what the query means.
		{ID: "mcp__github__merge_pull_request", Server: "github", Tool: "merge_pull_request",
			Description: "Merge a pull request in a repository"},
		// Shares a term, and is not what the query means.
		{ID: "mcp__other__combine_csv_columns", Server: "other", Tool: "combine_csv_columns",
			Description: "Combine two columns of a spreadsheet"},
	}
	// The query's vector points at the first entry and away from the second.
	vecs := map[string][]float32{
		"mcp__github__merge_pull_request": {1, 0},
		"mcp__other__combine_csv_columns": {0, 1},
	}
	qvec := []float32{0.98, 0.2}

	results, _ := Fuse("combine these branches once review passes", entries, vecs, qvec, 2)

	if len(results) == 0 {
		t.Fatal("no results")
	}
	if results[0].ID != "mcp__github__merge_pull_request" {
		got := make([]string, 0, len(results))
		for _, r := range results {
			got = append(got, r.ID)
		}
		t.Errorf("ranked %v; the best semantic match lost to an incidental term match", got)
	}
}
