package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbedPostsToTheConfiguredEndpointWithBearerAuthAndTheModel(t *testing.T) {
	var gotAuth, gotPath, gotModel string
	var gotInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = body.Model
		gotInputs = body.Input
		writeEmbeddings(w, len(body.Input))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test")
	vecs, err := c.Embed(context.Background(), []string{"alpha tool", "beta tool"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", gotPath)
	}
	if gotModel != "text-embedding-3-small" {
		t.Errorf("model = %q, want text-embedding-3-small", gotModel)
	}
	if len(gotInputs) != 2 {
		t.Fatalf("server saw %d inputs, want 2", len(gotInputs))
	}
	if len(vecs) != 2 {
		t.Fatalf("Embed returned %d vectors, want 2", len(vecs))
	}
}

func TestEmbedPreservesInputOrderAgainstAResponseIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&body)
		// Return the data out of input order; Embed must reassemble by index.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"index":1,"embedding":[2,2]},
			{"index":0,"embedding":[1,1]}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test")
	vecs, err := c.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vecs[0][0] != 1 || vecs[1][0] != 2 {
		t.Errorf("vecs = %v, want [[1 1] [2 2]] reassembled by index", vecs)
	}
}

func TestEmbedBatchesRatherThanSendingOneRequestPerText(t *testing.T) {
	var requests int
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&body)
		batchSizes = append(batchSizes, len(body.Input))
		writeEmbeddings(w, len(body.Input))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test")
	c.batchSize = 2
	texts := []string{"a", "b", "c", "d", "e"}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("Embed returned %d vectors, want %d", len(vecs), len(texts))
	}
	if requests != 3 {
		t.Errorf("requests = %d, want 3 batches of at most 2 for 5 texts", requests)
	}
	for _, n := range batchSizes {
		if n > 2 {
			t.Errorf("batch size %d exceeds configured 2", n)
		}
	}
}

func TestEmbedKeepsAlreadyFetchedVectorsWhenALaterBatchFails(t *testing.T) {
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

	c := NewClient(srv.URL, "sk-test")
	c.batchSize = 2
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c", "d"})
	if err == nil {
		t.Fatal("expected an error from the second batch's 500")
	}
	if len(vecs) != 4 {
		t.Fatalf("Embed returned %d slots, want 4 even on a partial failure", len(vecs))
	}
	if vecs[0] == nil || vecs[1] == nil {
		t.Errorf("vecs = %v, want the first batch's vectors kept despite the second batch failing", vecs)
	}
	if vecs[2] != nil || vecs[3] != nil {
		t.Errorf("vecs = %v, want the never-completed second batch's slots nil", vecs)
	}
}

func TestEmbedFailureIsSoft(t *testing.T) {
	// A client pointed at an address nothing listens on must return an error the
	// caller can check, not panic, so a caller can fall back to lexical-only
	// ranking rather than crash the refresh.
	c := NewClient("http://127.0.0.1:1", "sk-test")
	_, err := c.Embed(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("Embed against an unreachable gateway returned no error")
	}
}

func TestEmbedOnAnEmptyBatchMakesNoRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test")
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("vecs = %v, want none", vecs)
	}
	if called {
		t.Error("Embed made a request for an empty batch")
	}
}

func TestEmbedReturnsAnErrorOnANonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-bad")
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected an error on a 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention the status code", err)
	}
}

func writeEmbeddings(w http.ResponseWriter, n int) {
	type datum struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]datum, n)
	for i := range n {
		data[i] = datum{Index: i, Embedding: []float32{float32(i), float32(i)}}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Data []datum `json:"data"`
	}{Data: data})
}
