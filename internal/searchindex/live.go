package searchindex

import (
	"context"
	"crypto/sha256"
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

	applyMu      sync.Mutex
	mu           sync.RWMutex
	current      *Index
	keySHA256    [sha256.Size]byte
	hasKeySHA256 bool
}

func NewLive(state string, emb config.Embeddings, ranking config.Ranking) *Live {
	return &Live{state: state, emb: emb, ranking: ranking}
}

func (l *Live) ApplyAPIKey(apiKey string) error {
	keySHA256 := sha256.Sum256([]byte(apiKey))
	l.applyMu.Lock()
	defer l.applyMu.Unlock()

	l.mu.Lock()
	if l.current != nil && l.hasKeySHA256 && l.keySHA256 == keySHA256 {
		l.mu.Unlock()
		return nil
	}
	previous := l.current
	l.current = nil
	l.hasKeySHA256 = false
	l.mu.Unlock()

	if previous != nil {
		previous.Stop()
	}
	index := NewWithAPIKey(l.state, l.emb, l.ranking, apiKey)
	err := index.Load()
	l.mu.Lock()
	l.current = index
	l.keySHA256 = keySHA256
	l.hasKeySHA256 = true
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

func (l *Live) Status() Status {
	if index := l.index(); index != nil {
		return index.Status()
	}
	return Status{Model: l.emb.Model, QueueState: "idle"}
}

// Stop cancels and drains the active index queue.
func (l *Live) Stop() {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.mu.Lock()
	current := l.current
	l.current = nil
	l.mu.Unlock()
	if current != nil {
		current.Stop()
	}
}

func (l *Live) index() *Index {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}
