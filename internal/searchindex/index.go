package searchindex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/embedding"
	"github.com/ahodges22/mcpd/internal/rank"
)

type Index struct {
	client    *embedding.Client
	baseCache *embedding.Cache
	expansion *expansionCache
	gateway   *gateway
	reranker  string
	timeout   time.Duration

	refreshMu  sync.Mutex
	stateMu    sync.RWMutex
	base       map[string][]float32
	expanded   map[string][]float32
	missing    int
	unexpanded int

	queueMu   sync.Mutex
	queued    []catalog.Entry
	pending   bool
	latest    uint64
	running   bool
	stopped   bool
	queueWG   sync.WaitGroup
	queueCtx  context.Context
	stopQueue context.CancelFunc
}

func New(statePath string, emb config.Embeddings, ranking config.Ranking) *Index {
	return NewWithAPIKey(statePath, emb, ranking, emb.APIKey())
}

func NewWithAPIKey(statePath string, emb config.Embeddings, ranking config.Ranking, apiKey string) *Index {
	rerankTimeout := ranking.RerankTimeout()
	client := embedding.NewClient(emb.URL, apiKey, emb.Model)
	baseCache := embedding.NewCache(filepath.Join(statePath, "embeddings.json"), client.Model())
	queueCtx, stopQueue := context.WithCancel(context.Background())
	index := &Index{
		client:    client,
		baseCache: baseCache,
		base:      map[string][]float32{},
		expanded:  map[string][]float32{},
		queueCtx:  queueCtx,
		stopQueue: stopQueue,
	}
	if ranking.ExpansionModel != "" && ranking.RerankModel != "" && rerankTimeout > 0 {
		index.gateway = newGateway(emb.URL, apiKey)
		index.expansion = newExpansionCache(
			filepath.Join(statePath, "expansions.json"),
			ranking.ExpansionModel,
			client.Model(),
			baseCache.Dimension(),
		)
		index.reranker = ranking.RerankModel
		index.timeout = rerankTimeout
	}
	return index
}

func (i *Index) Load() error {
	var errs []error
	if err := i.baseCache.Load(); err != nil {
		errs = append(errs, err)
	}
	if i.expansion != nil {
		i.expansion.mu.Lock()
		i.expansion.dimension = i.baseCache.Dimension()
		i.expansion.mu.Unlock()
		if err := i.expansion.load(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (i *Index) Model() string { return i.client.Model() }

func (i *Index) Dimension() int { return i.baseCache.Dimension() }

func (i *Index) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return i.client.Embed(ctx, texts)
}

func (i *Index) Refresh(ctx context.Context, entries []catalog.Entry) {
	i.refresh(ctx, entries, 0)
}

func (i *Index) QueueRefresh(entries []catalog.Entry, budget time.Duration) {
	i.queueMu.Lock()
	if i.stopped {
		i.queueMu.Unlock()
		return
	}
	i.latest++
	i.queued = append([]catalog.Entry(nil), entries...)
	i.pending = true
	if i.running {
		i.queueMu.Unlock()
		return
	}
	i.running = true
	i.queueWG.Add(1)
	i.queueMu.Unlock()

	go func() {
		defer i.queueWG.Done()
		for {
			i.queueMu.Lock()
			if i.stopped {
				i.running = false
				i.queueMu.Unlock()
				return
			}
			entries := i.queued
			sequence := i.latest
			i.queued = nil
			i.pending = false
			i.queueMu.Unlock()

			ctx, cancel := context.WithTimeout(i.queueCtx, budget)
			i.refresh(ctx, entries, sequence)
			cancel()

			i.queueMu.Lock()
			if i.stopped {
				i.running = false
				i.queueMu.Unlock()
				return
			}
			if !i.pending {
				i.running = false
				i.queueMu.Unlock()
				return
			}
			i.queueMu.Unlock()
		}
	}()
}

func (i *Index) Stop() {
	i.queueMu.Lock()
	if !i.stopped {
		i.stopped = true
		i.queued = nil
		i.pending = false
		if i.stopQueue != nil {
			i.stopQueue()
		}
	}
	i.queueMu.Unlock()
	i.queueWG.Wait()
}

func (i *Index) refresh(ctx context.Context, entries []catalog.Entry, sequence uint64) {
	i.refreshMu.Lock()
	defer i.refreshMu.Unlock()

	base, missing := embedding.Vectorize(ctx, i.client, i.baseCache, entries)
	expanded := map[string][]float32{}
	expansionMissing := 0
	if i.expansion != nil {
		expanded, expansionMissing = i.expansion.ensure(ctx, entries, i.gateway, i.client)
	}
	if err := i.baseCache.Save(); err != nil {
		slog.Warn("save embedding cache", "error", err)
	}
	if i.expansion != nil {
		if err := i.expansion.save(); err != nil {
			slog.Warn("save expansion cache", "error", err)
		}
	}
	if sequence != 0 && !i.isLatest(sequence) {
		return
	}
	if missing == 0 {
		if dropped := i.baseCache.Prune(entries); dropped > 0 {
			slog.Info("pruned embeddings for tools no longer in the catalog", "count", dropped)
		}
	}
	if i.expansion != nil {
		if dropped := i.expansion.prune(entries); dropped > 0 {
			slog.Info("pruned expansions for tools no longer in the catalog", "count", dropped)
		}
		if expansionMissing > 0 {
			slog.Warn("tool query expansions are incomplete", "missing", expansionMissing, "tools", len(entries))
		}
	}
	if err := i.baseCache.Save(); err != nil {
		slog.Warn("save pruned embedding cache", "error", err)
	}
	if i.expansion != nil {
		if err := i.expansion.save(); err != nil {
			slog.Warn("save pruned expansion cache", "error", err)
		}
	}
	if sequence != 0 {
		i.queueMu.Lock()
		defer i.queueMu.Unlock()
		if sequence != i.latest {
			return
		}
	}
	i.stateMu.Lock()
	i.base, i.expanded, i.missing, i.unexpanded = base, expanded, missing, expansionMissing
	i.stateMu.Unlock()
}

func (i *Index) isLatest(sequence uint64) bool {
	i.queueMu.Lock()
	defer i.queueMu.Unlock()
	return sequence == i.latest
}

func (i *Index) Unvectorized() int {
	i.stateMu.RLock()
	defer i.stateMu.RUnlock()
	return i.missing
}

func (i *Index) Unexpanded() int {
	i.stateMu.RLock()
	defer i.stateMu.RUnlock()
	return i.unexpanded
}

func (i *Index) Vectors() (base, expanded map[string][]float32) {
	i.stateMu.RLock()
	defer i.stateMu.RUnlock()
	return i.base, i.expanded
}

func (i *Index) Search(ctx context.Context, query string, entries []catalog.Entry, limit int) ([]rank.Result, rank.Evidence, error) {
	vectors, err := i.client.Embed(ctx, []string{query})
	if err != nil {
		results, evidence := rank.Fuse(query, entries, nil, nil, limit)
		return results, evidence, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		results, evidence := rank.Fuse(query, entries, nil, nil, limit)
		return results, evidence, fmt.Errorf("embed query: gateway returned no vector")
	}
	return i.SearchVector(ctx, query, entries, vectors[0], limit)
}

func (i *Index) SearchVector(ctx context.Context, query string, entries []catalog.Entry, qvec []float32, limit int) ([]rank.Result, rank.Evidence, error) {
	base, expanded := i.Vectors()
	if i.gateway == nil {
		results, evidence := rank.Fuse(query, entries, base, qvec, limit)
		return results, evidence, nil
	}
	candidates := rank.Candidates(query, entries, base, expanded, qvec)
	rerankCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	reranked, err := i.gateway.rerank(rerankCtx, i.reranker, query, candidates, i.timeout)
	results, evidence := rank.Hybrid(query, entries, base, expanded, qvec, candidates, reranked, limit)
	if err != nil {
		return results, evidence, fmt.Errorf("rerank: %w", err)
	}
	return results, evidence, nil
}
