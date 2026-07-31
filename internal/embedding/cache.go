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
	"path/filepath"
	"sync"

	"github.com/ahodges/mcpd/internal/catalog"
)

// Cache persists embedding vectors keyed by a content hash of a tool's name,
// description, and schema, so a tool whose content has not changed is never
// re-embedded, and a renamed or reworded tool naturally misses.
//
// Every method is safe to call concurrently: the catalog fans out one refresh
// goroutine per backend, so one cache is shared by as many Vectorize calls as
// there are backends.
type Cache struct {
	path string
	// model is the gateway model these vectors came from. It is recorded on disk and
	// checked on load, because a calibrated cosine threshold is only valid for the model
	// that produced the vectors it was calibrated against. A swap to a different model at
	// the same dimension changes nothing the content hash can see, so without this the
	// daemon would keep serving a threshold that no longer means anything.
	model string

	saveMu sync.Mutex // serializes marshal-through-rename, so no save lands out of order

	mu      sync.Mutex
	vectors map[string][]float32
}

func NewCache(path, model string) *Cache {
	return &Cache{path: path, model: model, vectors: make(map[string][]float32)}
}

// document is the on-disk shape. The vectors used to be the whole file, with no record of
// what produced them; the header is what lets a load tell a reusable cache from one that
// belongs to a different model.
type document struct {
	Model     string               `json:"model"`
	Dimension int                  `json:"dimension"`
	Vectors   map[string][]float32 `json:"vectors"`
}

// Dimension reports the width of the vectors held, or zero when the cache is empty. Every
// vector in a cache has the same width, because a load discards any that do not match.
func (c *Cache) Dimension() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range c.vectors {
		return len(v)
	}
	return 0
}

// Prune drops vectors for content no longer in the catalog. The key is a content hash, so a
// reworded tool misses its old entry and never reads it again; without this the file grows
// by one dead vector per edit for the life of the machine.
func (c *Cache) Prune(entries []catalog.Entry) int {
	live := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		live[c.Key(e)] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := 0
	for key := range c.vectors {
		if _, ok := live[key]; !ok {
			delete(c.vectors, key)
			dropped++
		}
	}
	return dropped
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
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vectors[key]
	return v, ok
}

func (c *Cache) Put(key string, vec []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vectors[key] = vec
}

// Load reads the persisted cache; an absent file is a cold start, not an
// error.
//
// A cache written by a different model is discarded rather than merged. Discarding is
// deliberately whole-file: the alternative, folding the model into each key, would leave the
// superseded model's vectors on disk forever, and the point of the check is that a threshold
// calibrated against one model must never be applied to another's vectors. A vector whose
// width does not match the rest is dropped for the same reason. Both are a slower first
// refresh, which is the cheapest possible price for the guarantee.
func (c *Cache) Load() error {
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read embedding cache: %w", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse embedding cache: %w", err)
	}
	if doc.Vectors == nil {
		// A file from before the header existed, so what produced it is unknowable.
		slog.Info("embedding cache predates the model header; starting cold", "path", c.path)
		return nil
	}
	if doc.Model != c.model {
		slog.Warn("embedding cache was written by a different model; discarding it",
			"cached", doc.Model, "configured", c.model)
		return nil
	}
	vectors := make(map[string][]float32, len(doc.Vectors))
	dropped := 0
	for key, vec := range doc.Vectors {
		if doc.Dimension > 0 && len(vec) != doc.Dimension {
			dropped++
			continue
		}
		vectors[key] = vec
	}
	if dropped > 0 {
		slog.Warn("dropped cached vectors of the wrong width", "count", dropped, "dimension", doc.Dimension)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vectors = vectors
	return nil
}

// Save replaces the persisted cache atomically, holding saveMu across the whole
// marshal-through-rename so a save from one refresh cannot rename an older
// snapshot over a newer one, and a crash mid-write leaves no torn file for Load.
func (c *Cache) Save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()

	c.mu.Lock()
	doc := document{Model: c.model, Vectors: c.vectors}
	for _, v := range c.vectors {
		doc.Dimension = len(v)
		break
	}
	raw, err := json.Marshal(doc)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal embedding cache: %w", err)
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".embeddings-*")
	if err != nil {
		return fmt.Errorf("create temporary embedding cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write embedding cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write embedding cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return fmt.Errorf("replace embedding cache: %w", err)
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

	// Embed returns a full-length slice, filled in as far as it got, even on
	// error: a batch failure must not discard vectors earlier batches already
	// fetched and paid for.
	embedded, err := client.Embed(ctx, texts)
	if err != nil {
		slog.Warn("embeddings gateway error, degrading unresolved tools to lexical-only", "error", err)
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
