package rank

import (
	"math"
	"testing"

	"github.com/ahodges/mcpd/internal/catalog"
)

// referenceCatalog and the golden scores below were produced by running the
// original Python scorer (mcp-tool-search/src/proxy.py's query_terms/rank)
// against this exact catalog, so these tests pin the Go port against the
// prototype's real output rather than a re-derived expectation.
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

func TestLexicalMatchesThePythonPrototypeGoldenScores(t *testing.T) {
	entries := referenceCatalog()
	for _, tc := range []struct {
		query string
		want  []Result
	}{
		{
			query: "kubernetes pod logs",
			want: []Result{
				{ID: "mcp__art__kubectl_logs", Score: 5.505929512571311},
				{ID: "mcp__art__kubectl_get", Score: 0.5100312115660977},
			},
		},
		{
			// Exercises the docstring's own example: the join term "oncall" substring-
			// matches the squashed tool name "oncalls", so a two-word query with no
			// separator in the tool name still finds it.
			query: "on call schedule",
			want: []Result{
				{ID: "mcp__pagerduty__list_oncalls", Score: 6.01146315453949},
			},
		},
		{
			query: "weather forecast",
			want: []Result{
				{ID: "mcp__weather__get_weather", Score: 5.011051873981472},
			},
		},
		{
			query: "send a message to slack",
			want: []Result{
				{ID: "mcp__slack__send_message", Score: 9.878930837277759},
			},
		},
		{
			// "pod" is 3 characters, below the length>=4 gate the Python original
			// applies only to the name substring fallback; it exact-matches the
			// singular "pod" in kubectl_logs's description but does not touch
			// kubectl_get's description, which only contains the plural "pods".
			query: "pod",
			want: []Result{
				{ID: "mcp__art__kubectl_logs", Score: 1.252762968495368},
			},
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := Lexical(tc.query, entries)
			if len(got) != len(tc.want) {
				t.Fatalf("Lexical(%q) = %+v, want %+v", tc.query, got, tc.want)
			}
			for i, w := range tc.want {
				if got[i].ID != w.ID {
					t.Errorf("result[%d].ID = %q, want %q", i, got[i].ID, w.ID)
				}
				if math.Abs(got[i].Score-w.Score) > 1e-9 {
					t.Errorf("result[%d].Score = %v, want %v", i, got[i].Score, w.Score)
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
