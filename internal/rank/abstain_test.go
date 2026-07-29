package rank

import (
	"math"
	"strings"
	"testing"
)

// cosEv builds evidence carrying a cosine reading, with a lexical score that
// no assertion in the cosine tests depends on.
func cosEv(cosine float64) Evidence {
	return Evidence{BestCosine: cosine, BestLexical: 1, HasCosine: true}
}

func evs(cosines ...float64) []Evidence {
	out := make([]Evidence, len(cosines))
	for i, c := range cosines {
		out[i] = cosEv(c)
	}
	return out
}

func TestCalibratePutsTheCosineThresholdAtTheMidpointOfTheGap(t *testing.T) {
	// answerable floor 0.40, no-answer ceiling 0.20.
	answerable := evs(0.61, 0.40, 0.55)
	noAnswer := evs(0.11, 0.20, 0.07)

	got, err := Calibrate(answerable, noAnswer)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !got.Thresholds.Enabled {
		t.Fatal("separated bounds must enable abstention")
	}
	if want := 0.30; math.Abs(got.Thresholds.Cosine-want) > 1e-9 {
		t.Errorf("Cosine threshold = %v, want the gap midpoint %v", got.Thresholds.Cosine, want)
	}
	if got.Thresholds.Cosine <= 0.20 || got.Thresholds.Cosine >= 0.40 {
		t.Errorf("Cosine threshold %v is outside the gap (0.20, 0.40)", got.Thresholds.Cosine)
	}
	if b := got.CosineBounds; math.Abs(b.Floor-0.40) > 1e-9 || math.Abs(b.Ceiling-0.20) > 1e-9 || !b.Separated {
		t.Errorf("CosineBounds = %+v, want floor 0.40, ceiling 0.20, separated", b)
	}
}

// TestCalibrateDisablesAbstentionWhenItCannotFindAGap covers every way
// calibration can fail. A calibration function that only works on clean data
// is the one that quietly produces a garbage threshold later, so each case
// asserts the same three things: an error, abstention left disabled, and no
// threshold invented by clamping, widening, or midpointing an inverted
// interval.
func TestCalibrateDisablesAbstentionWhenItCannotFindAGap(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name             string
		answerable       []Evidence
		noAnswer         []Evidence
		wantErrContains  []string
		wantBoundsFilled bool
	}{
		{
			name:             "bounds overlap",
			answerable:       evs(0.55, 0.31, 0.60),
			noAnswer:         evs(0.12, 0.48, 0.20),
			wantErrContains:  []string{"0.31", "0.48"},
			wantBoundsFilled: true,
		},
		{
			name:             "bounds touch exactly",
			answerable:       evs(0.40, 0.72),
			noAnswer:         evs(0.40, 0.11),
			wantErrContains:  []string{"0.40"},
			wantBoundsFilled: true,
		},
		{
			name:            "no answerable cases",
			answerable:      nil,
			noAnswer:        evs(0.10),
			wantErrContains: []string{"answerable"},
		},
		{
			name:            "no no-answer cases",
			answerable:      evs(0.80),
			noAnswer:        nil,
			wantErrContains: []string{"no-answer"},
		},
		{
			name:            "an answerable case has no cosine evidence",
			answerable:      append(evs(0.80), Evidence{BestLexical: 4}),
			noAnswer:        evs(0.10),
			wantErrContains: []string{"answerable", "cosine"},
		},
		{
			name:            "a no-answer case has no cosine evidence",
			answerable:      evs(0.80),
			noAnswer:        append(evs(0.10), Evidence{BestLexical: 4}),
			wantErrContains: []string{"no-answer", "cosine"},
		},
		{
			name:            "a cosine reading is not a finite number",
			answerable:      append(evs(0.80), Evidence{BestCosine: nan, HasCosine: true}),
			noAnswer:        evs(0.10),
			wantErrContains: []string{"answerable"},
		},
		{
			name:            "a lexical reading is not a finite number",
			answerable:      evs(0.80),
			noAnswer:        append(evs(0.10), Evidence{BestCosine: 0.1, BestLexical: math.Inf(1), HasCosine: true}),
			wantErrContains: []string{"no-answer"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Calibrate(tc.answerable, tc.noAnswer)
			if err == nil {
				t.Fatalf("Calibrate returned no error; got %+v", got)
			}
			for _, want := range tc.wantErrContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if got.Thresholds.Enabled {
				t.Error("abstention must ship disabled rather than flag wrongly")
			}
			if got.Thresholds.Cosine != 0 {
				t.Errorf("Cosine threshold = %v, want no threshold at all", got.Thresholds.Cosine)
			}
			if got.CosineBounds.Separated {
				t.Error("CosineBounds reports a separation that was not found")
			}
			// Nothing may be flagged once calibration failed.
			if got.Thresholds.LowConfidence(cosEv(0)) {
				t.Error("disabled abstention flagged a query")
			}
			if tc.wantBoundsFilled && got.CosineBounds.Floor == 0 && got.CosineBounds.Ceiling == 0 {
				t.Error("the bounds that failed to separate must still be reported, so the absent gap can be recorded")
			}
		})
	}
}

// TestCalibrateReportsLexicalBoundsButNeverThresholdsThem pins the decision
// documented on Calibrate: the lexical bound is computed by the same fixed
// rule and reported, but a lexical overlap neither disables abstention nor
// contributes a threshold, and LowConfidence ignores BestLexical entirely.
func TestCalibrateReportsLexicalBoundsButNeverThresholdsThem(t *testing.T) {
	// Cosine separates cleanly; lexical is hopelessly overlapped.
	answerable := []Evidence{
		{BestCosine: 0.50, BestLexical: 3.0, HasCosine: true},
		{BestCosine: 0.60, BestLexical: 11.0, HasCosine: true},
	}
	noAnswer := []Evidence{
		{BestCosine: 0.10, BestLexical: 9.0, HasCosine: true},
		{BestCosine: 0.20, BestLexical: 1.0, HasCosine: true},
	}

	got, err := Calibrate(answerable, noAnswer)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !got.Thresholds.Enabled {
		t.Fatal("an overlapping lexical signal must not disable cosine abstention")
	}
	if b := got.LexicalBounds; math.Abs(b.Floor-3.0) > 1e-9 || math.Abs(b.Ceiling-9.0) > 1e-9 || b.Separated {
		t.Errorf("LexicalBounds = %+v, want floor 3.0, ceiling 9.0, not separated", b)
	}

	// Same cosine, wildly different lexical: the verdict must not move.
	weak := Evidence{BestCosine: 0.05, BestLexical: 0, HasCosine: true}
	weakButWordy := Evidence{BestCosine: 0.05, BestLexical: 500, HasCosine: true}
	if !got.Thresholds.LowConfidence(weak) || !got.Thresholds.LowConfidence(weakButWordy) {
		t.Error("a low cosine must be flagged regardless of the raw lexical score")
	}
	strong := Evidence{BestCosine: 0.95, BestLexical: 0, HasCosine: true}
	if got.Thresholds.LowConfidence(strong) {
		t.Error("a high cosine must not be flagged because the lexical score was low")
	}
}

func TestLowConfidenceFlagsOnlyCosineEvidenceBelowTheThreshold(t *testing.T) {
	enabled := Thresholds{Cosine: 0.30, Enabled: true}
	cases := []struct {
		name string
		t    Thresholds
		e    Evidence
		want bool
	}{
		{"below the threshold", enabled, cosEv(0.29), true},
		{"at the threshold", enabled, cosEv(0.30), false},
		{"above the threshold", enabled, cosEv(0.31), false},
		{"disabled by an absent gap", Thresholds{}, cosEv(0), false},
		{"no cosine evidence at all", enabled, Evidence{BestLexical: 7}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.LowConfidence(tc.e); got != tc.want {
				t.Errorf("LowConfidence(%+v) = %v, want %v", tc.e, got, tc.want)
			}
		})
	}
}

// TestRawLexicalScoresCarryNoAnswerabilitySignal is the executable evidence
// behind Calibrate thresholding cosine alone. Rewording one answerable query
// without changing which tool answers it moves its raw lexical score further
// than the entire distance between the answerable floor and the no-answer
// ceiling, so a bound placed in that window is set by the calibration set's
// query shapes rather than by the catalog. If the scorer ever changes so this
// stops holding, this test fails and the decision should be revisited.
func TestRawLexicalScoresCarryNoAnswerabilitySignal(t *testing.T) {
	entries := referenceCatalog()
	best := func(query string) (float64, string) {
		lex := Lexical(query, entries)
		if len(lex) == 0 {
			return 0, "none"
		}
		return lex[0].Score, lex[0].ID
	}

	terse, terseTop := best("slack")
	wordy, wordyTop := best("send a message to a slack channel")
	if terseTop != "mcp__slack__send_message" || wordyTop != "mcp__slack__send_message" {
		t.Fatalf("fixture invalid: both queries must be answered by send_message, got %q and %q", terseTop, wordyTop)
	}
	shapeDelta := math.Abs(wordy - terse)

	answerable := []string{
		"weather", "slack", "stream logs", "kubernetes pod logs", "on call schedule",
		"send a message to a slack channel", "get the current weather forecast for a location",
	}
	noAnswer := []string{
		"get my current location", "send a fax", "delete a namespace", "resize the current pod",
	}
	floor := math.Inf(1)
	for _, q := range answerable {
		if s, _ := best(q); s < floor {
			floor = s
		}
	}
	ceiling := math.Inf(-1)
	for _, q := range noAnswer {
		if s, _ := best(q); s > ceiling {
			ceiling = s
		}
	}

	t.Logf("answerable floor %.4f, no-answer ceiling %.4f, margin %.4f; rewording one answerable query moves it %.4f",
		floor, ceiling, floor-ceiling, shapeDelta)
	if shapeDelta <= floor-ceiling {
		t.Errorf("query shape moves the lexical score by %.4f, no longer more than the %.4f margin between answerable and no-answer: revisit thresholding lexical",
			shapeDelta, floor-ceiling)
	}
}
