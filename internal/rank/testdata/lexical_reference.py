#!/usr/bin/env python3
"""Differential fixture for internal/rank's port of the Python lexical scorer.

WORD, STOPWORDS, query_terms, and rank below are copied verbatim from
mcp-tool-search/src/proxy.py (the art-agent-scratch repo, outside this one) as
of the port in Task 6. They are not imported, because proxy.py pulls in an mcp
SDK this repo has no reason to depend on; copying the four functions avoids
that without touching their behaviour.

If Task 13's ranking regression gate ever fails, run this file to tell "the
port drifted from what was measured" apart from "the port was always wrong":

    python3 internal/rank/testdata/lexical_reference.py \
        > internal/rank/testdata/lexical_golden.tsv

lexical_test.go's TestLexicalMatchesThePythonPrototypeGoldenScores reads
lexical_golden.tsv and asserts Lexical() reproduces it exactly, so a diff in
the regenerated file versus the committed one is itself the signal that
something about the reference (not just the Go port) changed.

CATALOG here must stay in sync with referenceCatalog() in lexical_test.go by
hand; there is no automatic link between a Python literal and a Go one.
"""

import math
import re

WORD = re.compile(r"[a-z0-9]+")

STOPWORDS = frozenset(
    "a an the for to of in on is are was my me our who how do i and with that this it "
    "right now up all some any please can you".split()
)


def query_terms(query: str) -> list[str]:
    raw = WORD.findall(query.lower())
    terms = [w for w in raw if w not in STOPWORDS]
    terms += [raw[i] + raw[i + 1] for i in range(len(raw) - 1)]
    return list(dict.fromkeys(terms))


def rank(query: str, catalog: list[dict], limit: int = 10) -> list[dict]:
    terms = query_terms(query)
    if not terms:
        return []
    docs = []
    for e in catalog:
        name_t = set(WORD.findall(e["id"].lower()))
        name_t.add("".join(WORD.findall(e["tool"].lower())))
        docs.append((e, name_t, set(WORD.findall(e["description"].lower()))))
    n = len(docs) or 1
    df: dict[str, int] = {}
    for _, name_t, desc_t in docs:
        for term in name_t | desc_t:
            df[term] = df.get(term, 0) + 1
    scored = []
    unique_terms = set(terms)
    for entry, name_t, desc_t in docs:
        score = 0.0
        matched = set()
        for term in terms:
            idf = math.log(1 + n / (1 + df.get(term, 0)))
            if term in name_t:
                score += 3.0 * idf
                matched.add(term)
            elif len(term) >= 4 and any(term in tok for tok in name_t):
                score += 1.5 * idf
                matched.add(term)
            if term in desc_t:
                score += 1.0 * idf
                matched.add(term)
        if score > 0:
            coverage = len(matched) / len(unique_terms)
            scored.append((score * (0.4 + 0.6 * coverage), entry))
    scored.sort(key=lambda pair: (-pair[0], pair[1]["id"]))
    return [
        {"id": e["id"], "server": e["server"], "description": e["description"], "score": s}
        for s, e in scored[:limit]
    ]


# Mirrors referenceCatalog() in internal/rank/lexical_test.go.
CATALOG = [
    {
        "id": "mcp__art__kubectl_logs",
        "server": "art",
        "tool": "kubectl_logs",
        "description": "Stream or fetch logs from a Kubernetes pod by name and namespace.",
    },
    {
        "id": "mcp__art__kubectl_get",
        "server": "art",
        "tool": "kubectl_get",
        "description": "Get Kubernetes resources such as pods, deployments, and services.",
    },
    {
        "id": "mcp__pagerduty__list_oncalls",
        "server": "pagerduty",
        "tool": "list_oncalls",
        "description": "List who is on call for a given schedule.",
    },
    {
        "id": "mcp__weather__get_weather",
        "server": "weather",
        "tool": "get_weather",
        "description": "Get the current weather forecast for a location.",
    },
    {
        "id": "mcp__slack__send_message",
        "server": "slack",
        "tool": "send_message",
        "description": "Send a message to a Slack channel.",
    },
]

# Mirrors the query table in TestLexicalMatchesThePythonPrototypeGoldenScores.
QUERIES = [
    "kubernetes pod logs",
    "on call schedule",
    "weather forecast",
    "send a message to slack",
    "pod",
]

if __name__ == "__main__":
    for q in QUERIES:
        for row in rank(q, CATALOG, limit=10):
            print(f"{q}\t{row['id']}\t{repr(row['score'])}")
