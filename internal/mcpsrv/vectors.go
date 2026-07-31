package mcpsrv

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ahodges/mcpd/internal/catalog"
	"github.com/ahodges/mcpd/internal/embedding"
)

// VectorStore holds the catalog's embeddings and embeds a query on demand. It is the
// production Vectors, and it is what makes fusion and abstention live rather than dead
// code: without it every query reaches rank.Fuse with nil vectors.
type VectorStore struct {
	client *embedding.Client
	cache  *embedding.Cache

	mu           sync.RWMutex
	vecs         map[string][]float32
	unvectorized int
}

func NewVectorStore(client *embedding.Client, cache *embedding.Cache) *VectorStore {
	return &VectorStore{client: client, cache: cache, vecs: map[string][]float32{}}
}

func (v *VectorStore) Entries() map[string][]float32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.vecs
}

// Unvectorized reports how many catalog entries the gateway has not embedded, which the
// status surface lists: a silently lexical-only ranking is otherwise indistinguishable
// from a working one until the results are bad.
func (v *VectorStore) Unvectorized() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.unvectorized
}

// Refresh vectorizes the catalog and persists the cache. It is safe to call concurrently,
// which matters because it is driven from the catalog's post-commit hook and that fires
// from each refresh goroutine.
//
// It must never wait on a tool call. The same hook also fires from Drop, which runs inside
// a lifecycle teardown holding that backend's dispatch gate closed, so anything that needed
// a dispatch lease there would deadlock the daemon permanently rather than slowly.
func (v *VectorStore) Refresh(ctx context.Context, entries []catalog.Entry) {
	vecs, missing := embedding.Vectorize(ctx, v.client, v.cache, entries)
	v.mu.Lock()
	v.vecs, v.unvectorized = vecs, missing
	v.mu.Unlock()
	if err := v.cache.Save(); err != nil {
		slog.Warn("save embedding cache", "error", err)
	}
}

// Query embeds one search query, failing soft: a nil vector leaves the search lexical for
// that request rather than failing it.
func (v *VectorStore) Query(ctx context.Context, query string) []float32 {
	vecs, err := v.client.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		slog.Debug("embed query", "error", err)
		return nil
	}
	return vecs[0]
}
