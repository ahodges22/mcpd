package searchindex

import (
	"context"
	"sync"
	"time"

	"github.com/ahodges22/mcpd/internal/catalog"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/rank"
)

type Live struct {
	state   string
	emb     config.Embeddings
	ranking config.Ranking

	mu      sync.RWMutex
	current *Index
}

func NewLive(state string, emb config.Embeddings, ranking config.Ranking) *Live {
	return &Live{state: state, emb: emb, ranking: ranking}
}

func (l *Live) ApplyAPIKey(apiKey string) error {
	index := NewWithAPIKey(l.state, l.emb, l.ranking, apiKey)
	err := index.Load()
	l.mu.Lock()
	l.current = index
	l.mu.Unlock()
	return err
}

func (l *Live) Search(ctx context.Context, query string, entries []catalog.Entry, limit int) ([]rank.Result, rank.Evidence, error) {
	index := l.index()
	if index == nil {
		results, evidence := rank.Fuse(query, entries, nil, nil, limit)
		return results, evidence, nil
	}
	return index.Search(ctx, query, entries, limit)
}

func (l *Live) QueueRefresh(entries []catalog.Entry, budget time.Duration) {
	if index := l.index(); index != nil {
		index.QueueRefresh(entries, budget)
	}
}

func (l *Live) Unvectorized() int {
	if index := l.index(); index != nil {
		return index.Unvectorized()
	}
	return 0
}

func (l *Live) Model() string {
	if index := l.index(); index != nil {
		return index.Model()
	}
	return l.emb.Model
}

func (l *Live) index() *Index {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}
