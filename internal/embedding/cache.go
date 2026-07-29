package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/ahodges/mcpd/internal/catalog"
)

// Cache persists embedding vectors keyed by a content hash of a tool's name,
// description, and schema, so a tool whose content has not changed is never
// re-embedded, and a renamed or reworded tool naturally misses.
type Cache struct {
	path    string
	vectors map[string][]float32
}

func NewCache(path string) *Cache {
	return &Cache{path: path, vectors: make(map[string][]float32)}
}

func (c *Cache) Key(e catalog.Entry) string {
	h := sha256.New()
	h.Write([]byte(e.Tool))
	h.Write([]byte{0})
	h.Write([]byte(e.Description))
	h.Write([]byte{0})
	h.Write(e.Schema)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) Get(key string) ([]float32, bool) {
	v, ok := c.vectors[key]
	return v, ok
}

func (c *Cache) Put(key string, vec []float32) {
	c.vectors[key] = vec
}

// Load reads the persisted cache; an absent file is a cold start, not an
// error.
func (c *Cache) Load() error {
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read embedding cache: %w", err)
	}
	vectors := make(map[string][]float32)
	if err := json.Unmarshal(raw, &vectors); err != nil {
		return fmt.Errorf("parse embedding cache: %w", err)
	}
	c.vectors = vectors
	return nil
}

func (c *Cache) Save() error {
	raw, err := json.Marshal(c.vectors)
	if err != nil {
		return fmt.Errorf("marshal embedding cache: %w", err)
	}
	if err := os.WriteFile(c.path, raw, 0o600); err != nil {
		return fmt.Errorf("write embedding cache: %w", err)
	}
	return nil
}

// Vectorize returns a vector for every entry the cache already holds or the
// client embeds this call, and reports how many entries got no vector at
// all. A gateway that cannot be reached is not surfaced as an error: every
// entry that needed embedding is simply counted as unvectorized, which is
// how ranking degrades to lexical-only rather than failing a refresh.
func Vectorize(ctx context.Context, client *Client, cache *Cache, entries []catalog.Entry) (map[string][]float32, int) {
	vecs := make(map[string][]float32, len(entries))

	type pending struct {
		id  string
		key string
	}
	var missing []pending
	var texts []string
	for _, e := range entries {
		key := cache.Key(e)
		if v, ok := cache.Get(key); ok {
			vecs[e.ID] = v
			continue
		}
		missing = append(missing, pending{id: e.ID, key: key})
		texts = append(texts, embedText(e))
	}

	if len(missing) == 0 {
		return vecs, 0
	}

	embedded, err := client.Embed(ctx, texts)
	if err != nil {
		slog.Warn("embeddings gateway unreachable, degrading to lexical-only", "unvectorized", len(missing), "error", err)
		return vecs, len(missing)
	}

	unvectorized := 0
	for i, p := range missing {
		if embedded[i] == nil {
			unvectorized++
			continue
		}
		vecs[p.id] = embedded[i]
		cache.Put(p.key, embedded[i])
	}
	return vecs, unvectorized
}

func embedText(e catalog.Entry) string {
	if e.Description == "" {
		return e.Tool
	}
	return e.Tool + ": " + e.Description
}
