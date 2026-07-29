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

func TestFuseRespectsLimit(t *testing.T) {
	entries := referenceCatalog()
	got, _ := Fuse("get", entries, nil, nil, 1)
	if len(got) > 1 {
		t.Errorf("Fuse returned %d results, want at most 1", len(got))
	}
}
