// Command evalrank scores mcpd's tool ranking against a fixed query set and calibrates the
// abstention threshold.
//
// It reads the daemon's own catalog and embedding cache rather than standing up its own, so
// what it measures is what the daemon serves. It exits non-zero when the ranking misses its
// gate, and also when the run itself is not sound: an expected tool that is not in the
// catalog, or a catalog that is not fully vectorized, would both quietly produce a better
// number than the truth.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/embedding"
	"github.com/ahodges/mcpd/internal/rank"
)

// The acceptance gate, set over the expanded query set rather than over the fifteen the
// prototype shipped with. Those fifteen are carried forward inside it as a regression bar.
const (
	gateTop1 = 0.80
	gateTop3 = 0.95
	// limit is how many candidates the facade returns, so top-3 here is the real top-3.
	limit = 3
	// embedBudget covers embedding every query in the set on a cold run.
	embedBudget = 3 * time.Minute
)

//go:embed queries.json
var queryFS embed.FS

type answerable struct {
	Query    string   `json:"query"`
	Category string   `json:"category"`
	Accept   []string `json:"accept"`
	HeldOut  bool     `json:"held_out"`
}

type querySet struct {
	Answerable          []answerable `json:"answerable"`
	NoAnswerCalibration []string     `json:"no_answer_calibration"`
	NoAnswerValidation  []string     `json:"no_answer_validation"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "evalrank:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath   = flag.String("config", xdg("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file, for the embeddings gateway")
		statePath = flag.String("state", xdg("XDG_STATE_HOME", ".local/state", ""), "state directory holding catalog.json and embeddings.json")
		explain   = flag.Bool("explain", false, "for every miss, report where the expected tool sat in each ranker separately")
	)
	flag.Parse()

	set, err := loadQueries()
	if err != nil {
		return err
	}
	entries, err := loadCatalog(filepath.Join(*statePath, "catalog.json"))
	if err != nil {
		return err
	}
	fmt.Printf("catalog: %d tools\n", len(entries))

	// A query whose expected tool is not in the catalog is not a ranking failure, it is a
	// broken query. Scoring it as a miss would shrink the denominator's meaning and let the
	// eval drift as backends come and go, so the run stops instead.
	if missing := absent(set, entries); len(missing) > 0 {
		return fmt.Errorf("%d expected tool(s) are not in the catalog, so this run would score a different question:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	vecs, qvecs, err := vectors(*cfgPath, *statePath, set, entries)
	if err != nil {
		return err
	}

	base, held := split(set.Answerable)
	fmt.Printf("queries: %d answerable (%d held out), %d no-answer for calibration, %d for validation\n\n",
		len(set.Answerable), len(held), len(set.NoAnswerCalibration), len(set.NoAnswerValidation))

	if *explain {
		explainMisses(set.Answerable, entries, vecs, qvecs)
	}

	// The baseline, over the tuned-on set, reported before anything is calibrated.
	tuned := score("tuned-on", base, entries, vecs, qvecs)
	report(tuned)
	regression := score("prototype baseline (the 15 carried forward verbatim)", byCategory(set.Answerable, "baseline"), entries, vecs, qvecs)
	report(regression)

	cal, calErr := calibrate(set, base, entries, vecs, qvecs)
	fmt.Printf("\ncalibration\n")
	fmt.Printf("  cosine: answerable floor %.4f, no-answer ceiling %.4f, separated %v\n",
		cal.CosineBounds.Floor, cal.CosineBounds.Ceiling, cal.CosineBounds.Separated)
	fmt.Printf("  lexical (recorded, not used): floor %.4f, ceiling %.4f, separated %v\n",
		cal.LexicalBounds.Floor, cal.LexicalBounds.Ceiling, cal.LexicalBounds.Separated)
	if calErr != nil {
		fmt.Printf("  no usable gap: %v\n  abstention stays disabled, which is the finding\n", calErr)
	} else {
		fmt.Printf("  threshold: cosine %.4f, enabled %v\n", cal.Thresholds.Cosine, cal.Thresholds.Enabled)
	}

	// The held-out sets, scored exactly once, after the threshold is fixed. Everything above
	// this line is what the threshold was chosen from; everything below is the only evidence
	// about whether the choice generalises.
	fmt.Printf("\nheld out, scored once\n")
	out := score("held-out", held, entries, vecs, qvecs)
	report(out)
	falsePositives := flagged(cal.Thresholds, set.NoAnswerValidation, entries, vecs, qvecs)
	fmt.Printf("  no-answer validation: %d of %d correctly flagged as low confidence\n",
		falsePositives, len(set.NoAnswerValidation))
	fmt.Printf("  held-out versus tuned-on gap: top-1 %+.1f points, top-3 %+.1f points\n",
		100*(out.top1Rate()-tuned.top1Rate()), 100*(out.top3Rate()-tuned.top3Rate()))

	return gate(tuned, regression, out)
}

// gate decides the exit status. The whole answerable set is what the published gate is set
// over; the carried-forward fifteen must not regress below their recorded prototype scores.
func gate(tuned, regression, held result) error {
	var failures []string
	all := result{name: "all answerable"}
	for _, r := range []result{tuned, held} {
		all.top1 += r.top1
		all.top3 += r.top3
		all.total += r.total
	}
	if all.top1Rate() < gateTop1 {
		failures = append(failures, fmt.Sprintf("top-1 %.1f%% is below the %.0f%% gate", 100*all.top1Rate(), 100*gateTop1))
	}
	if all.top3Rate() < gateTop3 {
		failures = append(failures, fmt.Sprintf("top-3 %.1f%% is below the %.0f%% gate", 100*all.top3Rate(), 100*gateTop3))
	}
	// The prototype scored 11/15 top-1 and 15/15 top-3 over this same catalog. Fusion may not
	// do worse on the queries it was measured on.
	if regression.top1 < 11 {
		failures = append(failures, fmt.Sprintf("prototype regression: top-1 %d/15, the prototype scored 11/15", regression.top1))
	}
	if regression.top3 < 15 {
		failures = append(failures, fmt.Sprintf("prototype regression: top-3 %d/15, the prototype scored 15/15", regression.top3))
	}
	fmt.Printf("\noverall: top-1 %d/%d (%.1f%%), top-3 %d/%d (%.1f%%)\n",
		all.top1, all.total, 100*all.top1Rate(), all.top3, all.total, 100*all.top3Rate())
	if len(failures) > 0 {
		return errors.New("gate failed:\n  " + strings.Join(failures, "\n  "))
	}
	fmt.Println("gate passed")
	return nil
}

type result struct {
	name       string
	top1, top3 int
	total      int
	misses     []string
}

func (r result) top1Rate() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.top1) / float64(r.total)
}

func (r result) top3Rate() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.top3) / float64(r.total)
}

func score(name string, cases []answerable, entries []catalog.Entry, vecs, qvecs map[string][]float32) result {
	out := result{name: name, total: len(cases)}
	for _, c := range cases {
		results, _ := rank.Fuse(c.Query, entries, vecs, qvecs[c.Query], limit)
		accept := make(map[string]struct{}, len(c.Accept))
		for _, id := range c.Accept {
			accept[id] = struct{}{}
		}
		hit1, hit3 := false, false
		for i, r := range results {
			if _, ok := accept[r.ID]; !ok {
				continue
			}
			hit3 = true
			if i == 0 {
				hit1 = true
			}
		}
		if hit1 {
			out.top1++
		}
		if hit3 {
			out.top3++
		} else {
			got := make([]string, 0, len(results))
			for _, r := range results {
				got = append(got, r.ID)
			}
			out.misses = append(out.misses, fmt.Sprintf("[%s] %q got %s", c.Category, c.Query, strings.Join(got, ", ")))
		}
	}
	return out
}

func report(r result) {
	fmt.Printf("%s: top-1 %d/%d (%.1f%%), top-3 %d/%d (%.1f%%)\n",
		r.name, r.top1, r.total, 100*r.top1Rate(), r.top3, r.total, 100*r.top3Rate())
	for _, m := range r.misses {
		fmt.Println("  miss " + m)
	}
}

// calibrate collects the evidence from the tuned-on answerable set and the no-answer
// calibration set, never from either held-out set.
func calibrate(set querySet, base []answerable, entries []catalog.Entry, vecs, qvecs map[string][]float32) (rank.Calibration, error) {
	answerableEvidence := make([]rank.Evidence, 0, len(base))
	for _, c := range base {
		_, e := rank.Fuse(c.Query, entries, vecs, qvecs[c.Query], limit)
		answerableEvidence = append(answerableEvidence, e)
	}
	noAnswer := make([]rank.Evidence, 0, len(set.NoAnswerCalibration))
	for _, q := range set.NoAnswerCalibration {
		_, e := rank.Fuse(q, entries, vecs, qvecs[q], limit)
		noAnswer = append(noAnswer, e)
	}
	return rank.Calibrate(answerableEvidence, noAnswer)
}

// flagged counts how many no-answer queries the calibrated threshold correctly calls low
// confidence.
func flagged(t rank.Thresholds, queries []string, entries []catalog.Entry, vecs, qvecs map[string][]float32) int {
	n := 0
	for _, q := range queries {
		_, e := rank.Fuse(q, entries, vecs, qvecs[q], limit)
		if t.LowConfidence(e) {
			n++
		}
	}
	return n
}

func split(all []answerable) (base, held []answerable) {
	for _, c := range all {
		if c.HeldOut {
			held = append(held, c)
			continue
		}
		base = append(base, c)
	}
	return base, held
}

func byCategory(all []answerable, category string) []answerable {
	var out []answerable
	for _, c := range all {
		if c.Category == category {
			out = append(out, c)
		}
	}
	return out
}

func loadQueries() (querySet, error) {
	raw, err := queryFS.ReadFile("queries.json")
	if err != nil {
		return querySet{}, fmt.Errorf("read queries: %w", err)
	}
	var set querySet
	if err := json.Unmarshal(raw, &set); err != nil {
		return querySet{}, fmt.Errorf("parse queries: %w", err)
	}
	return set, nil
}

// loadCatalog reads the daemon's persisted catalog directly. Going through catalog.Catalog
// would need a live registry, and what is wanted here is exactly the tool set on disk.
func loadCatalog(path string) ([]catalog.Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var doc struct {
		Tools []catalog.Entry `json:"tools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if len(doc.Tools) == 0 {
		return nil, fmt.Errorf("%s holds no tools; start the daemon and let it refresh first", path)
	}
	return doc.Tools, nil
}

func absent(set querySet, entries []catalog.Entry) []string {
	have := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		have[e.ID] = struct{}{}
	}
	var missing []string
	for _, c := range set.Answerable {
		for _, id := range c.Accept {
			if _, ok := have[id]; !ok {
				missing = append(missing, fmt.Sprintf("%s (expected by %q)", id, c.Query))
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// vectors loads the catalog's embeddings and embeds every query in the set.
//
// It refuses to proceed on a partially warm cache. A cosine threshold calibrated while some
// tools have no vector is calibrated over a subset: the answerable floor is measured against
// whichever tools happened to be embedded, which biases it down and can erase a real gap
// entirely, and the resulting number would look like a finding rather than an artefact.
func vectors(cfgPath, statePath string, set querySet, entries []catalog.Entry) (vecs, qvecs map[string][]float32, err error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Embeddings.Enabled() {
		return nil, nil, fmt.Errorf("no embeddings gateway is configured, so there is nothing to calibrate")
	}
	client := embedding.NewClient(cfg.Embeddings.URL, cfg.Embeddings.APIKey(), cfg.Embeddings.Model)
	cache := embedding.NewCache(filepath.Join(statePath, "embeddings.json"), client.Model())
	if err := cache.Load(); err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), embedBudget)
	defer cancel()

	vecs, missing := embedding.Vectorize(ctx, client, cache, entries)
	if missing > 0 {
		return nil, nil, fmt.Errorf("%d of %d tools have no vector; calibrating over a subset biases the answerable floor down and can erase a real gap",
			missing, len(entries))
	}
	fmt.Printf("embeddings: %d vectors at %d dimensions, model %s\n", len(vecs), cache.Dimension(), client.Model())

	queries := make([]string, 0, len(set.Answerable)+len(set.NoAnswerCalibration)+len(set.NoAnswerValidation))
	for _, c := range set.Answerable {
		queries = append(queries, c.Query)
	}
	queries = append(queries, set.NoAnswerCalibration...)
	queries = append(queries, set.NoAnswerValidation...)

	embedded, err := client.Embed(ctx, queries)
	if err != nil {
		return nil, nil, fmt.Errorf("embed queries: %w", err)
	}
	if len(embedded) != len(queries) {
		return nil, nil, fmt.Errorf("gateway returned %d vectors for %d queries", len(embedded), len(queries))
	}
	qvecs = make(map[string][]float32, len(queries))
	for i, q := range queries {
		qvecs[q] = embedded[i]
	}
	return vecs, qvecs, nil
}

func xdg(env, fallback, file string) string {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, fallback)
	}
	dir := filepath.Join(base, "mcpd")
	if file == "" {
		return dir
	}
	return filepath.Join(dir, file)
}

// explainMisses reports, for every query that misses, where the expected tool sat in the
// lexical ranking and in the cosine ranking separately. Fusion can only promote what one of
// its inputs already ranked, so which input failed is the only thing worth knowing before
// changing anything: a tool the semantic ranker put 300th is not a fusion problem.
func explainMisses(cases []answerable, entries []catalog.Entry, vecs, qvecs map[string][]float32) {
	fmt.Println("explain")
	for _, c := range cases {
		results, _ := rank.Fuse(c.Query, entries, vecs, qvecs[c.Query], limit)
		accept := map[string]struct{}{}
		for _, id := range c.Accept {
			accept[id] = struct{}{}
		}
		hit := false
		for _, r := range results {
			if _, ok := accept[r.ID]; ok {
				hit = true
			}
		}
		if hit {
			continue
		}
		lex := rank.Lexical(c.Query, entries)
		cos := rank.Cosine(entries, vecs, qvecs[c.Query])
		fmt.Printf("  %q\n", c.Query)
		for _, id := range c.Accept {
			fmt.Printf("    %-70s lexical #%s  cosine #%s\n", id, place(lex, id), place(cos, id))
		}
	}
}

func place(results []rank.Result, id string) string {
	for i, r := range results {
		if r.ID == id {
			return fmt.Sprintf("%d", i+1)
		}
	}
	return "absent"
}
