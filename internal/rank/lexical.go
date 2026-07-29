// Package rank scores catalog entries against a query: a lexical scorer
// ported from the Python prototype, and reciprocal rank fusion of that
// scorer with an embedding-similarity ordering.
package rank

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/ahodges/mcpd/internal/catalog"
)

// Result is one scored catalog entry, returned by both Lexical and Fuse.
// Score means different things for each: a lexical weight for the former,
// a fused reciprocal-rank sum for the latter.
type Result struct {
	ID          string
	Server      string
	Description string
	Score       float64
}

var wordPattern = regexp.MustCompile(`[a-z0-9]+`)

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "for": true, "to": true, "of": true,
	"in": true, "on": true, "is": true, "are": true, "was": true, "my": true,
	"me": true, "our": true, "who": true, "how": true, "do": true, "i": true,
	"and": true, "with": true, "that": true, "this": true, "it": true,
	"right": true, "now": true, "up": true, "all": true, "some": true,
	"any": true, "please": true, "can": true, "you": true,
}

// queryTerms is a port of proxy.py's query_terms: content words plus
// adjacent-word joins of every raw word including stopwords, so "on call"
// can match a squashed tool name like "oncalls".
func queryTerms(query string) []string {
	raw := wordPattern.FindAllString(strings.ToLower(query), -1)
	terms := make([]string, 0, len(raw))
	for _, w := range raw {
		if !stopwords[w] {
			terms = append(terms, w)
		}
	}
	for i := 0; i+1 < len(raw); i++ {
		terms = append(terms, raw[i]+raw[i+1])
	}
	return dedup(terms)
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func wordSet(s string) map[string]bool {
	words := wordPattern.FindAllString(strings.ToLower(s), -1)
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

type lexDoc struct {
	entry catalog.Entry
	nameT map[string]bool
	descT map[string]bool
}

func buildDocs(entries []catalog.Entry) []lexDoc {
	docs := make([]lexDoc, len(entries))
	for i, e := range entries {
		nameT := wordSet(e.ID)
		squashed := strings.Join(wordPattern.FindAllString(strings.ToLower(e.Tool), -1), "")
		nameT[squashed] = true
		docs[i] = lexDoc{entry: e, nameT: nameT, descT: wordSet(e.Description)}
	}
	return docs
}

func documentFrequency(docs []lexDoc) map[string]int {
	df := make(map[string]int)
	for _, d := range docs {
		union := make(map[string]bool, len(d.nameT)+len(d.descT))
		for w := range d.nameT {
			union[w] = true
		}
		for w := range d.descT {
			union[w] = true
		}
		for w := range union {
			df[w]++
		}
	}
	return df
}

func substringInAny(term string, set map[string]bool) bool {
	for tok := range set {
		if strings.Contains(tok, term) {
			return true
		}
	}
	return false
}

// Lexical is a port of proxy.py's rank: keyword matching with idf weighting,
// a 3x bonus for an exact name match versus 1x for description, a 1.5x
// substring fallback for name matches on terms of at least 4 characters
// (never for description matches), and a coverage bonus favouring breadth of
// term matches over one strong match. It is deterministic and returns every
// entry that scored above zero, sorted by score descending then id
// ascending, with no limit: Fuse needs a full ranking to compute reciprocal
// ranks over.
func Lexical(query string, entries []catalog.Entry) []Result {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil
	}
	docs := buildDocs(entries)
	n := len(docs)
	if n == 0 {
		n = 1
	}
	df := documentFrequency(docs)
	uniqueTerms := len(terms)

	type scored struct {
		entry catalog.Entry
		score float64
	}
	var results []scored
	for _, d := range docs {
		score := 0.0
		matched := make(map[string]bool, len(terms))
		for _, term := range terms {
			idf := math.Log(1 + float64(n)/float64(1+df[term]))
			if d.nameT[term] {
				score += 3.0 * idf
				matched[term] = true
			} else if len(term) >= 4 && substringInAny(term, d.nameT) {
				score += 1.5 * idf
				matched[term] = true
			}
			if d.descT[term] {
				score += 1.0 * idf
				matched[term] = true
			}
		}
		if score > 0 {
			coverage := float64(len(matched)) / float64(uniqueTerms)
			results = append(results, scored{d.entry, score * (0.4 + 0.6*coverage)})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].entry.ID < results[j].entry.ID
	})

	out := make([]Result, len(results))
	for i, r := range results {
		out[i] = Result{ID: r.entry.ID, Server: r.entry.Server, Description: r.entry.Description, Score: r.score}
	}
	return out
}
