package rank

import (
	"fmt"
	"math"
)

// Thresholds is the calibrated abstention configuration, produced by
// Calibrate and consumed by the search facade. Its zero value is disabled,
// which is the right default for a daemon whose thresholds were never
// calibrated: no flag at all beats a flag that is noise.
type Thresholds struct {
	Cosine  float64
	Enabled bool
}

// LowConfidence reports whether a query's evidence is weak enough that its
// candidates should be returned flagged rather than presented as answers.
//
// It reads the raw best cosine similarity only: never the fused score, which
// encodes rank position and ranker agreement rather than relevance, and never
// the raw lexical score, for the reason recorded on Calibrate.
//
// Evidence with no cosine reading is never flagged. In the lexical-only
// degraded mode there is no comparable signal to judge against, so abstention
// goes quiet rather than flagging every query.
func (t Thresholds) LowConfidence(e Evidence) bool {
	return t.Enabled && e.HasCosine && e.BestCosine < t.Cosine
}

// Bounds is what the selection rule found for one signal: the highest
// threshold that still leaves every answerable case above it, the lowest that
// still leaves every no-answer case below it, and whether the first exceeds
// the second.
type Bounds struct {
	Floor     float64
	Ceiling   float64
	Separated bool
}

// Calibration is a calibration run's full outcome. Thresholds is what the
// facade runs with; the two Bounds are what the rule found, reported so that
// an absent gap can be recorded as the finding it is.
type Calibration struct {
	Thresholds    Thresholds
	CosineBounds  Bounds
	LexicalBounds Bounds
}

// Calibrate applies one rule per signal, fixed in advance of scoring any
// threshold so that the outcome cannot be rationalised after the fact: the
// highest threshold leaving every answerable case above it, the lowest
// leaving every no-answer case below it, and the midpoint of the two when the
// first exceeds the second. When they do not separate there is no gap, which
// is a finding about the signal rather than a number to adjust: Calibrate
// returns the bounds it found with abstention left disabled and an error
// naming the overlap. It never clamps, widens, or midpoints an inverted
// interval into a threshold.
//
// Only the cosine bound becomes a threshold. The raw lexical score is a sum
// of idf weights over matched terms, so its magnitude tracks query length and
// term rarity rather than whether any tool serves the query: rewording one
// query without changing its answer moves the score further than the whole
// answerable-to-no-answer margin (TestRawLexicalScoresCarryNoAnswerabilitySignal
// measures this). LexicalBounds is therefore reported for the record and read
// by nothing.
func Calibrate(answerable, noAnswer []Evidence) (Calibration, error) {
	if len(answerable) == 0 || len(noAnswer) == 0 {
		return Calibration{}, fmt.Errorf("calibration needs both sets: %d answerable, %d no-answer", len(answerable), len(noAnswer))
	}
	if err := checkUsable("answerable", answerable); err != nil {
		return Calibration{}, err
	}
	if err := checkUsable("no-answer", noAnswer); err != nil {
		return Calibration{}, err
	}

	c := Calibration{
		CosineBounds:  boundsOf(answerable, noAnswer, func(e Evidence) float64 { return e.BestCosine }),
		LexicalBounds: boundsOf(answerable, noAnswer, func(e Evidence) float64 { return e.BestLexical }),
	}
	if !c.CosineBounds.Separated {
		return c, fmt.Errorf("no separating cosine gap: answerable floor %.4f does not exceed no-answer ceiling %.4f, abstention disabled",
			c.CosineBounds.Floor, c.CosineBounds.Ceiling)
	}
	c.Thresholds = Thresholds{
		Cosine:  (c.CosineBounds.Floor + c.CosineBounds.Ceiling) / 2,
		Enabled: true,
	}
	return c, nil
}

// checkUsable rejects a calibration set that cannot support a bound. A case with
// no cosine reading would drag the bound as if it were a reading of zero, and
// a non-finite one silently loses every min/max comparison it takes part in.
func checkUsable(set string, evidence []Evidence) error {
	for i, e := range evidence {
		if !e.HasCosine {
			return fmt.Errorf("%s case %d has no cosine evidence", set, i)
		}
		if !isFinite(e.BestCosine) || !isFinite(e.BestLexical) {
			return fmt.Errorf("%s case %d has a non-finite score: cosine %v, lexical %v", set, i, e.BestCosine, e.BestLexical)
		}
	}
	return nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func boundsOf(answerable, noAnswer []Evidence, of func(Evidence) float64) Bounds {
	floor := of(answerable[0])
	for _, e := range answerable[1:] {
		if v := of(e); v < floor {
			floor = v
		}
	}
	ceiling := of(noAnswer[0])
	for _, e := range noAnswer[1:] {
		if v := of(e); v > ceiling {
			ceiling = v
		}
	}
	return Bounds{Floor: floor, Ceiling: ceiling, Separated: floor > ceiling}
}
