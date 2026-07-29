// Package catalog flattens every backend's tools into one canonically
// identified set, refreshed per backend by a coalescing loop.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
)

const (
	// DefaultTTL is both the period of the TTL trigger and the cap on the refresh
	// backoff, so a backend reporting its tool list changed continuously is polled
	// at this rate rather than spun on.
	DefaultTTL = time.Hour

	defaultDebounce    = 250 * time.Millisecond
	defaultBackoffBase = 250 * time.Millisecond
	// defaultListTimeout bounds the list itself. Without it a backend that accepts
	// the connection and never answers parks its refresh loop for good: config's
	// per-backend timeout is optional and defaults to none. It is added to the
	// backend's own handshake budget rather than replacing it, so a slow cold start
	// is never truncated by a deadline the user did not configure.
	defaultListTimeout = 30 * time.Second
)

// CanonicalID is the only identifier a tool is addressed by. The format is fixed:
// existing agent permission hooks match on the inner tool name.
func CanonicalID(server, tool string) string { return "mcp__" + server + "__" + tool }

type Entry struct {
	ID          string               `json:"id"`
	Server      string               `json:"server"`
	Tool        string               `json:"tool"`
	Description string               `json:"description,omitempty"`
	Schema      json.RawMessage      `json:"schema,omitempty"`
	Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
}

// lister is the part of *backend.Backend the catalog reads.
type lister interface {
	ListTools(context.Context) ([]*mcp.Tool, error)
	Generation() uint64
	// ConnectTimeout is the handshake budget the read deadline must sit on top of.
	ConnectTimeout() time.Duration
}

// backends is the part of *backend.Registry the catalog reads.
type backends interface {
	names() []string
	lister(name string) (lister, bool)
}

var _ lister = (*backend.Backend)(nil)

type registrySource struct{ r *backend.Registry }

func (s registrySource) names() []string { return s.r.Names() }

func (s registrySource) lister(name string) (lister, bool) {
	b, ok := s.r.Get(name)
	if !ok {
		return nil, false
	}
	return b, true
}

type tuning struct {
	debounce    time.Duration
	backoffBase time.Duration
	ttl         time.Duration
	listTimeout time.Duration
}

// serverState is one backend's refresh bookkeeping. triggers is what decides
// whether to read again; the generation counter, which catches a lifecycle
// transition invalidating a read, belongs to the backend.
type serverState struct {
	triggers uint64
	running  bool
	armed    bool
	timer    *time.Timer
	cancel   context.CancelFunc
	done     chan struct{}
}

type Catalog struct {
	backends backends
	path     string
	tune     tuning

	saveMu sync.Mutex // serializes marshal-through-rename, so no save lands out of order
	// beforeRename is a test seam: it forces two saves to interleave, which is the
	// only way to observe an out-of-order rename deterministically.
	beforeRename func()

	mu      sync.Mutex
	idle    sync.Cond
	index   map[string]Entry
	errs    map[string]string
	servers map[string]*serverState
}

// New builds a catalog over reg, persisted at path.
func New(reg *backend.Registry, path string) *Catalog {
	return newCatalog(registrySource{reg}, path, tuning{
		debounce:    defaultDebounce,
		backoffBase: defaultBackoffBase,
		ttl:         DefaultTTL,
		listTimeout: defaultListTimeout,
	})
}

func newCatalog(src backends, path string, tune tuning) *Catalog {
	c := &Catalog{
		backends: src,
		path:     path,
		tune:     tune,
		index:    make(map[string]Entry),
		errs:     make(map[string]string),
		servers:  make(map[string]*serverState),
	}
	c.idle.L = &c.mu
	return c
}

func (c *Catalog) Entries() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sortedLocked()
}

func (c *Catalog) Lookup(id string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.index[id]
	return e, ok
}

// Errors reports, per backend, why that backend's tools are absent.
func (c *Catalog) Errors() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.errs)
}

// Start runs the TTL trigger until ctx is done: every backend is re-listed on
// each tick. This is what recovers a backend that was unreachable when the daemon
// started, and what keeps a backend that never sends tool-list-changed (the
// notification is optional) from going stale. ctx stops the ticker; it does not
// bound the refreshes the ticker starts.
func (c *Catalog) Start(ctx context.Context) {
	go func() {
		tick := time.NewTicker(c.tune.ttl)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// Trigger rather than RefreshAll: a tick must never wait on a refresh
				// loop. A backend that notifies continuously has a loop that by design
				// never exits, and waiting on it would stop the TTL trigger for every
				// other backend for the life of the process.
				for _, name := range c.backends.names() {
					c.Trigger(name)
				}
			}
		}
	}()
}

// Trigger records that server's tool list may have changed. The counter advances
// immediately, so a read already in flight cannot satisfy the trigger: the
// trigger means that read is answering a superseded question. Starting a fresh
// round is debounced, so a burst becomes one refresh.
func (c *Catalog) Trigger(server string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.backends.lister(server); !ok {
		return
	}
	st := c.stateLocked(server)
	st.triggers++
	if st.running || st.armed {
		return
	}
	st.armed = true
	st.timer = time.AfterFunc(c.tune.debounce, func() { c.debounced(server, st) })
}

// Refresh requests a read of server without waiting out the debounce, and returns
// once its refresh loop has finished. ctx bounds the wait, not the work: a caller
// that gives up must not cancel a refresh other triggers are relying on, and a
// read that is already running is bounded by the list timeout instead. If a loop
// is mid-backoff the wait includes the remainder of that sleep.
func (c *Catalog) Refresh(ctx context.Context, server string) {
	c.mu.Lock()
	st := c.stateLocked(server)
	st.triggers++
	c.disarmLocked(st)
	c.startLocked(server, st)
	done := st.done
	c.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// RefreshAll fans out one independent refresh per backend. Each commits on its
// own, so a slow or failing backend neither delays nor discards another's tools.
func (c *Catalog) RefreshAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range c.backends.names() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Refresh(ctx, name)
		}()
	}
	wg.Wait()
}

// StopRefresh cancels any pending or in-flight refresh for server and returns
// only once the loop has exited, so a caller tearing the backend down cannot
// race a commit.
func (c *Catalog) StopRefresh(server string) {
	c.mu.Lock()
	st, ok := c.servers[server]
	if !ok {
		c.mu.Unlock()
		return
	}
	c.disarmLocked(st)
	if st.running {
		done, cancel := st.done, st.cancel
		c.mu.Unlock()
		cancel()
		<-done
		return
	}
	c.mu.Unlock()
}

// Drop evicts server's tools and its error record. A disable calls it once its
// refresh loop has been stopped, so nothing can commit the tools it removes.
func (c *Catalog) Drop(server string) {
	c.mu.Lock()
	dropped := c.dropLocked(server)
	delete(c.errs, server)
	c.mu.Unlock()
	if dropped {
		c.persist()
	}
}

// WaitIdle returns when no backend has a refresh pending or in flight.
func (c *Catalog) WaitIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.busyLocked() {
		c.idle.Wait()
	}
}

func (c *Catalog) debounced(server string, st *serverState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !st.armed {
		return // disarmed while this timer waited for the lock
	}
	st.armed = false
	c.startLocked(server, st)
	c.idle.Broadcast()
}

// startLocked runs the loop on a catalog-owned context rather than the context of
// whichever caller happened to start it. A manual re-index whose HTTP request is
// abandoned must not cancel a loop that a notification is waiting on; StopRefresh
// is the only way to end one.
func (c *Catalog) startLocked(server string, st *serverState) {
	if st.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	st.done = make(chan struct{})
	go c.loop(ctx, cancel, server, st)
}

// loop reads until a read completes with the trigger counter unchanged. A
// trigger that arrives during a read is answered by the next read, never by the
// one it interrupted.
func (c *Catalog) loop(ctx context.Context, cancel context.CancelFunc, server string, st *serverState) {
	defer cancel()
	for round := 1; ; round++ {
		c.mu.Lock()
		seen := st.triggers
		c.mu.Unlock()

		c.read(ctx, server)

		c.mu.Lock()
		stop := ctx.Err() != nil || st.triggers == seen
		if stop {
			c.finishLocked(st)
		}
		c.mu.Unlock()
		if stop {
			return
		}

		if !sleep(ctx, c.backoff(round)) {
			c.mu.Lock()
			c.finishLocked(st)
			c.mu.Unlock()
			return
		}
	}
}

func (c *Catalog) read(loop context.Context, server string) {
	l, ok := c.backends.lister(server)
	if !ok {
		return
	}
	// The backend's handshake budget is added rather than shared: a read that has to
	// connect gets the connect budget the config allows plus the list budget, so this
	// deadline can never cut a handshake short.
	ctx, cancel := context.WithTimeout(loop, l.ConnectTimeout()+c.tune.listTimeout)
	defer cancel()

	tools, err := l.ListTools(ctx)
	switch {
	case errors.Is(err, backend.ErrStaleGeneration), errors.Is(err, backend.ErrDisabled), loop.Err() != nil:
		// Superseded rather than failed: a stale generation, a backend the user
		// turned off, or a stop we asked for.
		// Neither marks the backend down nor evicts the tools it is still serving.
		// The loop context is what is checked, so a read that exceeded the list
		// timeout still falls through to the failure branch below.
		return
	case err != nil:
		c.exclude(server, err)
		return
	}
	// The generation is sampled after the read, because ListTools may have had to
	// connect and a connect is itself a lifecycle transition that moves it.
	c.commit(server, l, l.Generation(), tools)
}

func (c *Catalog) commit(server string, l lister, gen uint64, tools []*mcp.Tool) {
	entries := flatten(server, tools)

	c.mu.Lock()
	if l.Generation() != gen {
		c.mu.Unlock()
		return
	}
	c.dropLocked(server)
	for _, e := range entries {
		c.index[e.ID] = e
	}
	delete(c.errs, server)
	c.mu.Unlock()

	c.persist()
}

func (c *Catalog) exclude(server string, cause error) {
	c.mu.Lock()
	dropped := c.dropLocked(server)
	c.errs[server] = cause.Error()
	c.mu.Unlock()

	if dropped {
		c.persist()
	}
}

func (c *Catalog) dropLocked(server string) bool {
	dropped := false
	for id, e := range c.index {
		if e.Server == server {
			delete(c.index, id)
			dropped = true
		}
	}
	return dropped
}

func (c *Catalog) finishLocked(st *serverState) {
	st.running = false
	st.cancel = nil
	close(st.done)
	c.idle.Broadcast()
}

func (c *Catalog) disarmLocked(st *serverState) {
	if !st.armed {
		return
	}
	st.armed = false
	st.timer.Stop()
	c.idle.Broadcast()
}

func (c *Catalog) stateLocked(server string) *serverState {
	st, ok := c.servers[server]
	if !ok {
		st = &serverState{}
		c.servers[server] = st
	}
	return st
}

func (c *Catalog) busyLocked() bool {
	for _, st := range c.servers {
		if st.running || st.armed {
			return true
		}
	}
	return false
}

func (c *Catalog) backoff(round int) time.Duration {
	d := c.tune.backoffBase << min(round-1, 32)
	if d <= 0 || d > c.tune.ttl {
		return c.tune.ttl
	}
	return d
}

func (c *Catalog) sortedLocked() []Entry {
	out := slices.Collect(maps.Values(c.index))
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type document struct {
	Tools []Entry `json:"tools"`
}

// Load reads the persisted catalog, so search answers before any backend has
// been re-listed. An absent file is a first run, not an error. Entries for
// backends that are no longer configured are discarded: nothing else would ever
// remove them, and search would offer tools no backend can serve.
func (c *Catalog) Load() error {
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}
	configured := c.backends.names()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.index = make(map[string]Entry, len(doc.Tools))
	for _, e := range doc.Tools {
		if slices.Contains(configured, e.Server) {
			c.index[e.ID] = e
		}
	}
	return nil
}

// Save replaces the persisted catalog atomically. It holds saveMu across the
// whole marshal-through-rename, because concurrent saves from a fan-out would
// otherwise let an older snapshot rename over a newer one.
func (c *Catalog) Save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()

	c.mu.Lock()
	raw, err := json.Marshal(document{Tools: c.sortedLocked()})
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	if c.beforeRename != nil {
		c.beforeRename()
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return fmt.Errorf("create temporary catalog: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write catalog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write catalog: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return fmt.Errorf("replace catalog: %w", err)
	}
	return nil
}

func (c *Catalog) persist() {
	if err := c.Save(); err != nil {
		slog.Error("persist catalog", "path", c.path, "error", err)
	}
}

func flatten(server string, tools []*mcp.Tool) []Entry {
	out := make([]Entry, 0, len(tools))
	for _, t := range tools {
		out = append(out, Entry{
			ID:          CanonicalID(server, t.Name),
			Server:      server,
			Tool:        t.Name,
			Description: t.Description,
			Schema:      rawSchema(t.InputSchema),
			Annotations: t.Annotations,
		})
	}
	return out
}

func rawSchema(schema any) json.RawMessage {
	if schema == nil {
		return nil
	}
	if raw, ok := schema.(json.RawMessage); ok {
		return raw
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return raw
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
