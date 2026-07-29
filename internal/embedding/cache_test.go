package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ahodges/mcpd/internal/catalog"
)

func entry(tool, description string) catalog.Entry {
	return catalog.Entry{
		ID:          catalog.CanonicalID("srv", tool),
		Server:      "srv",
		Tool:        tool,
		Description: description,
	}
}

func TestCacheKeyChangesWhenNameDescriptionOrSchemaChanges(t *testing.T) {
	base := entry("weather", "get the weather")
	renamed := base
	renamed.Tool = "forecast"
	reworded := base
	reworded.Description = "get the forecast"
	reschema := base
	reschema.Schema = json.RawMessage(`{"type":"object"}`)

	c := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	baseKey := c.Key(base)
	for _, tc := range []struct {
		name string
		e    catalog.Entry
	}{
		{"renamed", renamed},
		{"reworded", reworded},
		{"reschema", reschema},
	} {
		if c.Key(tc.e) == baseKey {
			t.Errorf("%s: key unchanged, want it to differ from the base entry's key", tc.name)
		}
	}
	if c.Key(base) != baseKey {
		t.Error("Key is not deterministic for an unchanged entry")
	}
}

func TestCacheGetPutRoundTrips(t *testing.T) {
	c := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	e := entry("weather", "get the weather")
	key := c.Key(e)

	if _, ok := c.Get(key); ok {
		t.Fatal("Get hit before any Put")
	}
	c.Put(key, []float32{1, 2, 3})
	got, ok := c.Get(key)
	if !ok || len(got) != 3 || got[0] != 1 {
		t.Fatalf("Get after Put = %v, %v", got, ok)
	}
}

func TestCacheSaveThenLoadRoundTripsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := NewCache(path)
	e := entry("weather", "get the weather")
	key := c.Key(e)
	c.Put(key, []float32{0.5, -0.25})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewCache(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := reloaded.Get(key)
	if !ok || len(got) != 2 || got[0] != 0.5 || got[1] != -0.25 {
		t.Fatalf("reloaded cache = %v, %v, want the saved vector", got, ok)
	}
}

func TestCacheLoadOfAnAbsentFileIsNotAnError(t *testing.T) {
	c := NewCache(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err := c.Load(); err != nil {
		t.Fatalf("Load of an absent cache: %v", err)
	}
}

func TestVectorizeCachesByContentHashSoAnUnchangedToolIsNeverReembedded(t *testing.T) {
	var embedCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		var body struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&body)
		writeEmbeddings(w, len(body.Input))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "sk-test")
	cache := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	entries := []catalog.Entry{entry("weather", "get the weather")}

	vecs, unvectorized := Vectorize(context.Background(), client, cache, entries)
	if unvectorized != 0 || len(vecs) != 1 {
		t.Fatalf("first vectorize: vecs=%v unvectorized=%d", vecs, unvectorized)
	}
	if embedCalls != 1 {
		t.Fatalf("embed calls = %d, want 1", embedCalls)
	}

	vecs, unvectorized = Vectorize(context.Background(), client, cache, entries)
	if unvectorized != 0 || len(vecs) != 1 {
		t.Fatalf("second vectorize: vecs=%v unvectorized=%d", vecs, unvectorized)
	}
	if embedCalls != 1 {
		t.Errorf("embed calls = %d after a second Vectorize of the same entry, want still 1", embedCalls)
	}
}

func TestVectorizeReportsUnvectorizedWhenTheGatewayIsUnreachable(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "sk-test")
	cache := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	entries := []catalog.Entry{
		entry("weather", "get the weather"),
		entry("logs", "get pod logs"),
	}

	vecs, unvectorized := Vectorize(context.Background(), client, cache, entries)
	if len(vecs) != 0 {
		t.Errorf("vecs = %v, want none when the gateway is unreachable", vecs)
	}
	if unvectorized != 2 {
		t.Errorf("unvectorized = %d, want 2", unvectorized)
	}
}

func TestVectorizeServesAlreadyCachedToolsWithAnUnreachableGateway(t *testing.T) {
	// A warm cache works offline: an already-embedded tool ranks with full
	// fidelity even when the gateway that produced its vector is now down;
	// only a genuinely new tool is counted unvectorized.
	cache := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	warm := entry("weather", "get the weather")
	cache.Put(cache.Key(warm), []float32{1, 2, 3})

	newTool := entry("logs", "get pod logs")
	client := NewClient("http://127.0.0.1:1", "sk-test")

	vecs, unvectorized := Vectorize(context.Background(), client, cache, []catalog.Entry{warm, newTool})
	if got, ok := vecs[warm.ID]; !ok || len(got) != 3 {
		t.Fatalf("warm tool vector = %v, %v, want the cached vector served offline", got, ok)
	}
	if unvectorized != 1 {
		t.Errorf("unvectorized = %d, want 1 (only the new tool)", unvectorized)
	}
}
