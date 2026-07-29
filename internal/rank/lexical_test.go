package rank

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ahodges/mcpd/internal/catalog"
)

// referenceCatalog mirrors CATALOG in testdata/lexical_reference.py, a
// verbatim extraction of proxy.py's scorer, run to produce
// testdata/lexical_golden.tsv. Keep the two catalogs in sync by hand: there
// is no automatic link between a Go literal and a Python one. If this
// changes, regenerate the golden file per that script's header comment.
func referenceCatalog() []catalog.Entry {
	return []catalog.Entry{
		{
			ID:          "mcp__art__kubectl_logs",
			Server:      "art",
			Tool:        "kubectl_logs",
			Description: "Stream or fetch logs from a Kubernetes pod by name and namespace.",
		},
		{
			ID:          "mcp__art__kubectl_get",
			Server:      "art",
			Tool:        "kubectl_get",
			Description: "Get Kubernetes resources such as pods, deployments, and services.",
		},
		{
			ID:          "mcp__pagerduty__list_oncalls",
			Server:      "pagerduty",
			Tool:        "list_oncalls",
			Description: "List who is on call for a given schedule.",
		},
		{
			ID:          "mcp__weather__get_weather",
			Server:      "weather",
			Tool:        "get_weather",
			Description: "Get the current weather forecast for a location.",
		},
		{
			ID:          "mcp__slack__send_message",
			Server:      "slack",
			Tool:        "send_message",
			Description: "Send a message to a Slack channel.",
		},
	}
}

// goldenRow is one line of testdata/lexical_golden.tsv: query, id, and score
// in the rank order the real Python scorer produced.
type goldenRow struct {
	query string
	id    string
	score float64
}

func loadGolden(t *testing.T, path string) []goldenRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open golden fixture: %v", err)
	}
	defer f.Close()

	var rows []goldenRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("malformed golden row: %q", line)
		}
		score, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			t.Fatalf("parse golden score %q: %v", parts[2], err)
		}
		rows = append(rows, goldenRow{query: parts[0], id: parts[1], score: score})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	return rows
}

// TestLexicalMatchesThePythonPrototypeGoldenScores replays
// testdata/lexical_golden.tsv, which testdata/lexical_reference.py (a
// verbatim extraction of proxy.py's scorer) produced against
// referenceCatalog, so this pins the Go port to the prototype's actual
// output rather than a re-derived expectation. Two of the covered queries
// exercise the port's most subtle behaviours: "on call schedule" is the
// docstring's own example of the join term "oncall" substring-matching the
// squashed tool name "oncalls", and "pod" (3 characters, below the
// length>=4 gate) exact-matches the singular "pod" in kubectl_logs's
// description but not kubectl_get's plural "pods", because that length
// gate applies only to the name substring fallback, never to description
// matching.
func TestLexicalMatchesThePythonPrototypeGoldenScores(t *testing.T) {
	entries := referenceCatalog()
	golden := loadGolden(t, "testdata/lexical_golden.tsv")

	var queries []string
	want := make(map[string][]goldenRow)
	for _, row := range golden {
		if _, ok := want[row.query]; !ok {
			queries = append(queries, row.query)
		}
		want[row.query] = append(want[row.query], row)
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			got := Lexical(query, entries)
			rows := want[query]
			if len(got) != len(rows) {
				t.Fatalf("Lexical(%q) = %+v, want %d results", query, got, len(rows))
			}
			for i, row := range rows {
				if got[i].ID != row.id {
					t.Errorf("result[%d].ID = %q, want %q", i, got[i].ID, row.id)
				}
				if math.Abs(got[i].Score-row.score) > 1e-9 {
					t.Errorf("result[%d].Score = %v, want %v", i, got[i].Score, row.score)
				}
			}
		})
	}
}

func TestLexicalOnAllStopwordsMatchesNothing(t *testing.T) {
	// A single stopword produces no query terms at all in the Python original
	// (query_terms returns [] before rank ever looks at the catalog), which this
	// pins by returning no results at all rather than one for every doc.
	got := Lexical("the", referenceCatalog())
	if len(got) != 0 {
		t.Errorf("Lexical(%q) = %+v, want no results", "the", got)
	}
}

func TestLexicalFavoursBreadthOfTermCoverageOverASingleStrongMatch(t *testing.T) {
	entries := []catalog.Entry{
		{ID: "mcp__a__broad", Server: "a", Tool: "broad", Description: "reads pod logs for troubleshooting"},
		{ID: "mcp__a__narrow", Server: "a", Tool: "narrow", Description: "kubernetes kubernetes kubernetes"},
	}
	got := Lexical("kubernetes pod logs", entries)
	if len(got) == 0 || got[0].ID != "mcp__a__broad" {
		t.Fatalf("Lexical ranked coverage below a single repeated term: %+v", got)
	}
}

func TestLexicalRanksSortStableByScoreThenID(t *testing.T) {
	entries := []catalog.Entry{
		{ID: "mcp__z__tool", Server: "z", Tool: "tool", Description: "widget widget"},
		{ID: "mcp__a__tool", Server: "a", Tool: "tool", Description: "widget widget"},
	}
	got := Lexical("widget", entries)
	if len(got) != 2 || got[0].ID != "mcp__a__tool" || got[1].ID != "mcp__z__tool" {
		t.Fatalf("tie-break by ID failed: %+v", got)
	}
}
