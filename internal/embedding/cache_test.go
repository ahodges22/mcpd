package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

func TestOneCacheServesTheCatalogsPerBackendFanOut(t *testing.T) {
	// catalog.RefreshAll runs one goroutine per backend, so a cache wired into a
	// per-backend refresh is written by all of them at once, while a save of the
	// whole cache runs alongside. Unsynchronised that is a concurrent map write.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&body)
		writeEmbeddings(w, len(body.Input))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "sk-test")
	path := filepath.Join(t.TempDir(), "cache.json")
	cache := NewCache(path)

	const backends = 8
	var wg sync.WaitGroup
	for b := range backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := entry(fmt.Sprintf("tool%d", b), fmt.Sprintf("description %d", b))
			if _, unvectorized := Vectorize(context.Background(), client, cache, []catalog.Entry{e}); unvectorized != 0 {
				t.Errorf("backend %d: unvectorized = %d, want 0", b, unvectorized)
			}
			if err := cache.Save(); err != nil {
				t.Errorf("backend %d: Save: %v", b, err)
			}
		}()
	}
	wg.Wait()

	if err := cache.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded := NewCache(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for b := range backends {
		e := entry(fmt.Sprintf("tool%d", b), fmt.Sprintf("description %d", b))
		if _, ok := reloaded.Get(reloaded.Key(e)); !ok {
			t.Errorf("tool%d's vector did not survive the concurrent fan-out and save", b)
		}
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

func TestVectorizeKeepsPartialProgressAndReportsAnAccurateCountAcrossBatches(t *testing.T) {
	// The real catalog is around 583 tools, far more than one batch: a cold
	// start that hits a gateway hiccup partway through must keep the vectors
	// it already paid for and count only what is genuinely still missing,
	// not the whole catalog.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&body)
		writeEmbeddings(w, len(body.Input))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "sk-test")
	client.batchSize = 2
	cache := NewCache(filepath.Join(t.TempDir(), "cache.json"))
	entries := []catalog.Entry{
		entry("t0", "d0"), entry("t1", "d1"), entry("t2", "d2"), entry("t3", "d3"), entry("t4", "d4"),
	}

	vecs, unvectorized := Vectorize(context.Background(), client, cache, entries)
	if len(vecs) != 2 {
		t.Fatalf("vecs = %v, want the 2 vectors the successful first batch fetched", vecs)
	}
	if unvectorized != 3 {
		t.Fatalf("unvectorized = %d, want 3 (the failed batch plus the batch never attempted)", unvectorized)
	}
	if _, ok := cache.Get(cache.Key(entries[0])); !ok {
		t.Error("a vector fetched before the failure was not cached")
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
