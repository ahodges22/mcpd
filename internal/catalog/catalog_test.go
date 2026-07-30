package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/config"
	"github.com/ahodges/mcpd/internal/testfake"
)

var fastTuning = tuning{
	debounce:    time.Millisecond,
	backoffBase: time.Millisecond,
	ttl:         20 * time.Millisecond,
	listTimeout: 5 * time.Second,
}

func TestCanonicalIdsFlattenEveryBackendThroughARealRegistry(t *testing.T) {
	reg := httpRegistry(t, testfake.New("github", tool("create_pull_request")), testfake.New("infra", tool("kubectl_logs")))
	c := New(reg, filepath.Join(t.TempDir(), "catalog.json"))
	t.Cleanup(func() { stopAll(c, reg.Names()) })

	c.RefreshAll(t.Context())

	if got, want := ids(c), []string{"mcp__github__create_pull_request", "mcp__infra__kubectl_logs"}; !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	e, ok := c.Lookup("mcp__github__create_pull_request")
	if !ok {
		t.Fatal("Lookup missed an id the catalog just reported")
	}
	if e.Server != "github" || e.Tool != "create_pull_request" {
		t.Errorf("entry = %+v, want server github and tool create_pull_request", e)
	}
	if !strings.Contains(string(e.Schema), "properties") {
		t.Errorf("schema = %s, want the upstream input schema so describe_tool costs no upstream call", e.Schema)
	}
}

func TestDeadBackendDoesNotSinkTheCatalog(t *testing.T) {
	// A failed read excludes the backend; a superseded one is not a failure and
	// must leave both its tools and its error record alone.
	for _, tc := range []struct {
		name     string
		err      error
		wantErr  string
		wantKept bool
	}{
		{name: "a failed read excludes the backend", err: errSpawn, wantErr: errSpawn.Error()},
		{name: "a superseded read changes nothing", err: fmt.Errorf("backend beta: list tools: %w", backend.ErrStaleGeneration), wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beta := serving(tool("beta_tool"))
			c := newTestCatalog(t, stubSource{
				"alpha": serving(tool("kubectl_logs"), tool("kubectl_get")),
				"beta":  beta,
			}, fastTuning)

			// Refreshed in order rather than through RefreshAll: the failure has to be
			// processed after the successes, or an implementation that discards the whole
			// catalog on one failure passes on scheduling luck.
			c.Refresh(t.Context(), "alpha")
			c.Refresh(t.Context(), "beta")
			beta.set(tc.err)
			c.Refresh(t.Context(), "beta")

			want := []string{"mcp__alpha__kubectl_get", "mcp__alpha__kubectl_logs"}
			if tc.wantKept {
				want = append(want, "mcp__beta__beta_tool")
				slices.Sort(want)
			}
			if got := ids(c); !slices.Equal(got, want) {
				t.Errorf("ids = %v, want %v", got, want)
			}
			if got := c.Errors()["beta"]; !strings.Contains(got, tc.wantErr) || (tc.wantErr == "" && got != "") {
				t.Errorf("recorded error for beta = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestADownBackendIsRelistedWhenItRecovers(t *testing.T) {
	alpha := failing(errSpawn)
	tune := fastTuning
	tune.ttl = 20 * time.Millisecond
	c := newTestCatalog(t, stubSource{"alpha": alpha}, tune)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c.RefreshAll(ctx)
	if c.Errors()["alpha"] == "" {
		t.Fatal("a backend that failed to list was not recorded")
	}

	c.Start(ctx)
	alpha.set(nil, tool("kubectl_logs"))

	// Nothing else can re-list it: tool-list-changed needs a live session and a
	// reconnect needs one to have been lost. Without the TTL trigger a backend that
	// was down at startup stays invisible until an operator re-indexes by hand.
	waitFor(t, func() bool {
		_, ok := c.Lookup("mcp__alpha__kubectl_logs")
		return ok
	})
	if got := c.Errors()["alpha"]; got != "" {
		t.Errorf("recorded error %q survived a successful re-list", got)
	}
}

func TestAChattyBackendDoesNotStopTheTTLTriggerForOthers(t *testing.T) {
	// chatty reports a change on every read, so by design its refresh loop never
	// exits. A tick that waited on that loop would freeze the TTL trigger for every
	// other backend for the life of the process.
	var c *Catalog
	chatty := serving(tool("chatty_tool"))
	chatty.onRead = func() { c.Trigger("chatty") }
	quiet := serving(tool("quiet_tool"))
	tune := fastTuning
	tune.backoffBase, tune.ttl = 10*time.Millisecond, 20*time.Millisecond
	c = newTestCatalog(t, stubSource{"chatty": chatty, "quiet": quiet}, tune)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	waitFor(t, func() bool {
		_, ok := c.Lookup("mcp__quiet__quiet_tool")
		return ok
	})

	// quiet never notifies, so only a later tick can notice this.
	quiet.set(nil, tool("renamed_tool"))
	waitFor(t, func() bool {
		_, ok := c.Lookup("mcp__quiet__renamed_tool")
		return ok
	})
}

func TestTheListDeadlineDoesNotTruncateTheHandshake(t *testing.T) {
	// A backend allowed a longer handshake than the catalog's list budget must not be
	// cut off at the list budget: a cold npx fetch is slower than any list.
	const handshake = 200 * time.Millisecond
	slow := serving(tool("kubectl_logs"))
	slow.block, slow.connect = make(chan struct{}), handshake
	tune := fastTuning
	tune.listTimeout = 20 * time.Millisecond
	c := newTestCatalog(t, stubSource{"alpha": slow}, tune)

	start := time.Now()
	c.Refresh(t.Context(), "alpha")

	if elapsed := time.Since(start); elapsed < handshake {
		t.Errorf("the read was abandoned after %s, but this backend's handshake budget alone is %s: a backend with a slow cold start would never be catalogued", elapsed, handshake)
	}
	if c.Errors()["alpha"] == "" {
		t.Error("a read that exhausted its whole budget was not recorded as a failure")
	}
}

func TestAHungListBecomesARecordedFailure(t *testing.T) {
	hung := serving(tool("kubectl_logs"))
	hung.block = make(chan struct{}) // never released
	tune := fastTuning
	tune.listTimeout = 20 * time.Millisecond
	c := newTestCatalog(t, stubSource{"alpha": hung}, tune)

	done := make(chan struct{})
	go func() { c.Refresh(context.Background(), "alpha"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered tools/list parked the refresh loop, so WaitIdle and RefreshAll would never return")
	}

	if c.Errors()["alpha"] == "" {
		t.Error("a backend that never answered records no error, so it looks healthy while never refreshing")
	}
	c.WaitIdle()
}

func TestAPathologicalBackendBacksOffAndIsCappedAtTheTTL(t *testing.T) {
	measured := newCatalog(stubSource{}, filepath.Join(t.TempDir(), "catalog.json"), tuning{
		backoffBase: 250 * time.Millisecond,
		ttl:         time.Second,
	})
	for i, want := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, time.Second, time.Second} {
		if got := measured.backoff(i + 1); got != want {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, want)
		}
	}

	// A backend that reports a change on every read must be polled at the cap, not
	// spun on.
	var c *Catalog
	alpha := serving(tool("kubectl_logs"))
	alpha.onRead = func() { c.Trigger("alpha") }
	tune := fastTuning
	tune.backoffBase, tune.ttl = 10*time.Millisecond, 20*time.Millisecond
	c = newTestCatalog(t, stubSource{"alpha": alpha}, tune)

	go c.Refresh(context.Background(), "alpha")
	time.Sleep(200 * time.Millisecond)

	switch n := alpha.calls.Load(); {
	case n > 30:
		t.Errorf("reads = %d in 200ms with a 10ms base and a 20ms cap: the loop is spinning rather than backing off", n)
	case n < 2:
		t.Errorf("reads = %d: the loop stopped instead of polling at the cap", n)
	}
}

func TestTriggerDuringRefreshCausesExactlyOneFollowUpWhoseResultWins(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"), tool("kubectl_get"))
	gate := holdLists(t, fake, 1)
	c := newTestCatalog(t, stubSource{"alpha": listerFor(t, fake)}, fastTuning)

	done := make(chan struct{})
	go func() { c.RefreshAll(context.Background()); close(done) }()
	waitFor(t, func() bool { return fake.ListCalls.Load() == 1 })

	fake.SetTools(tool("kubectl_logs"))
	c.Trigger("alpha")
	c.Trigger("alpha")
	c.Trigger("alpha")
	gate.release(1)
	<-done
	c.WaitIdle()

	if n := fake.ListCalls.Load(); n != 2 {
		t.Errorf("tools/list calls = %d, want 2: one in flight plus one coalesced follow-up", n)
	}
	if got, want := ids(c), []string{"mcp__alpha__kubectl_logs"}; !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v: the follow-up read is the one that must be committed", got, want)
	}
}

func TestTriggerDuringTheFollowUpCausesAThirdRead(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	gate := holdLists(t, fake, 3)
	c := newTestCatalog(t, stubSource{"alpha": listerFor(t, fake)}, fastTuning)

	done := make(chan struct{})
	go func() { c.RefreshAll(context.Background()); close(done) }()

	waitFor(t, func() bool { return fake.ListCalls.Load() == 1 })
	c.Trigger("alpha")
	gate.release(1)

	waitFor(t, func() bool { return fake.ListCalls.Load() == 2 })
	c.Trigger("alpha")
	gate.release(2)

	waitFor(t, func() bool { return fake.ListCalls.Load() == 3 })
	gate.release(3)
	<-done
	c.WaitIdle()

	if n := fake.ListCalls.Load(); n != 3 {
		t.Errorf("tools/list calls = %d, want 3: the loop must converge on the trigger counter, not stop at a fixed bound", n)
	}
}

func TestAnAbandonedRefreshDoesNotDropAPendingTrigger(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"), tool("kubectl_get"))
	gate := holdLists(t, fake, 1)
	c := newTestCatalog(t, stubSource{"alpha": listerFor(t, fake)}, fastTuning)

	// A manual re-index arrives on a request context and the client disconnects
	// while the read is in flight. The notification that landed in between belongs
	// to the daemon, not to that request.
	ctx, cancel := context.WithCancel(context.Background())
	go c.Refresh(ctx, "alpha")
	waitFor(t, func() bool { return fake.ListCalls.Load() == 1 })

	fake.SetTools(tool("kubectl_logs"))
	c.Trigger("alpha")
	cancel()
	gate.release(1)

	waitFor(t, func() bool { return fake.ListCalls.Load() == 2 })
	c.WaitIdle()

	if got, want := ids(c), []string{"mcp__alpha__kubectl_logs"}; !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v: the follow-up read must still commit", got, want)
	}
}

func TestABurstOfTriggersCollapsesIntoOneRead(t *testing.T) {
	fake := testfake.New("alpha", tool("kubectl_logs"))
	tune := fastTuning
	tune.debounce = 500 * time.Millisecond
	c := newTestCatalog(t, stubSource{"alpha": listerFor(t, fake)}, tune)

	// Spaced, because triggers issued back to back collapse on the trigger counter
	// alone: each read is fast enough to finish between them, so only the debounce
	// stops five triggers becoming five reads.
	for range 5 {
		c.Trigger("alpha")
		if n := fake.ListCalls.Load(); n != 0 {
			t.Fatalf("tools/list calls = %d before the debounce window elapsed, want 0", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.WaitIdle()

	if n := fake.ListCalls.Load(); n != 1 {
		t.Errorf("tools/list calls = %d, want 1: a burst inside the debounce window is one refresh", n)
	}
}

func TestASlowBackendDoesNotBlockAFastOne(t *testing.T) {
	// The slow backend sorts first, so a refresh that fanned out in name order
	// rather than concurrently would never reach the fast one.
	slow := serving(tool("slow_tool"))
	slow.block = make(chan struct{})
	c := newTestCatalog(t, stubSource{
		"blocked": slow,
		"quick":   serving(tool("kubectl_logs"), tool("kubectl_get")),
	}, fastTuning)

	done := make(chan struct{})
	go func() { c.RefreshAll(context.Background()); close(done) }()
	waitFor(t, func() bool { return len(c.Entries()) == 2 })

	if got := c.Errors()["blocked"]; got != "" {
		t.Errorf("the slow backend was recorded as failed while still reading: %q", got)
	}
	close(slow.block)
	<-done

	if n := len(c.Entries()); n != 3 {
		t.Errorf("entries = %d, want 3 once the slow backend answers", n)
	}
}

func TestALifecycleTransitionAfterTheReadDoesNotCommit(t *testing.T) {
	l := &scriptedLister{
		reads: [][]*mcp.Tool{{tool("kubectl_logs"), tool("kubectl_get")}, {tool("replacement")}},
		gens:  []uint64{1, 1, 2, 3},
	}
	c := newTestCatalog(t, stubSource{"alpha": l}, fastTuning)

	c.Refresh(t.Context(), "alpha")
	c.Refresh(t.Context(), "alpha")

	if got, want := ids(c), []string{"mcp__alpha__kubectl_get", "mcp__alpha__kubectl_logs"}; !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v: a read superseded by a lifecycle transition must not commit", got, want)
	}
	if got := c.Errors()["alpha"]; got != "" {
		t.Errorf("recorded error %q: being superseded is not a backend failure", got)
	}
}

func TestAPersistedCatalogAnswersBeforeAnyBackendIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	written := newCatalog(stubSource{
		"alpha": serving(tool("kubectl_logs")),
		"ghost": serving(tool("removed_tool")),
	}, path, fastTuning)
	written.RefreshAll(t.Context())

	// ghost is gone from the config on restart, as after a rename.
	restarted := newCatalog(stubSource{"alpha": refusingLister{t: t}}, path, fastTuning)
	if err := restarted.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, ok := restarted.Lookup("mcp__alpha__kubectl_logs"); !ok {
		t.Error("the persisted catalog does not answer, so a restart serves nothing until every backend is re-listed")
	}
	if _, ok := restarted.Lookup("mcp__ghost__removed_tool"); ok {
		t.Error("a de-configured backend's tools were restored; nothing ever removes them and every call to one fails at dispatch")
	}
}

func TestAnOlderSnapshotCannotRenameOverANewerOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	src := stubSource{"alpha": serving(tool("kubectl_logs")), "beta": serving(tool("beta_tool"))}
	c := newCatalog(src, path, fastTuning)
	t.Cleanup(func() { stopAll(c, src.names()) })

	// The first save marshals one backend and parks before its rename. If a second
	// save can marshal both backends and land while it waits, the parked save then
	// renames the older document over the newer one, and a restart loses a backend
	// that was listed successfully.
	var once sync.Once
	parked, resume := make(chan struct{}), make(chan struct{})
	c.beforeRename = func() {
		once.Do(func() {
			close(parked)
			<-resume
		})
	}

	go c.Refresh(context.Background(), "alpha")
	<-parked
	second := make(chan struct{})
	go func() { c.Refresh(context.Background(), "beta"); close(second) }()
	select {
	case <-second:
	case <-time.After(200 * time.Millisecond):
		// Serialised behind the parked save, which is the point.
	}
	close(resume)
	<-second

	reloaded := newCatalog(src, path, fastTuning)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := len(reloaded.Entries()), 2; got != want {
		t.Errorf("persisted %d entries, want %d: an older snapshot renamed over a newer one", got, want)
	}
}

func TestThePostCommitHookRunsOnEveryMutationWithTheCommitAlreadyVisible(t *testing.T) {
	// The pass-through endpoint's tool set tracks the catalog through this hook and
	// nothing else, so a path that mutates the index without firing it leaves the
	// endpoint advertising tools no backend serves.
	alpha := serving(tool("kubectl_logs"))
	c := newTestCatalog(t, stubSource{"alpha": alpha}, fastTuning)

	var seen [][]string
	c.OnCommit(func() {
		// Entries takes c.mu, so a hook fired inside the commit's critical section
		// deadlocks here rather than quietly reading a half-applied index.
		seen = append(seen, ids(c))
	})

	c.Refresh(t.Context(), "alpha") // commit
	alpha.set(errors.New("upstream gone"))
	c.Refresh(t.Context(), "alpha") // exclude
	alpha.set(nil, tool("kubectl_logs"))
	c.Refresh(t.Context(), "alpha") // commit again
	c.Drop("alpha")                 // drop

	want := [][]string{{"mcp__alpha__kubectl_logs"}, {}, {"mcp__alpha__kubectl_logs"}, {}}
	if len(seen) != len(want) {
		t.Fatalf("the hook fired %d times (%v), want once per mutation: %v", len(seen), seen, want)
	}
	for i := range want {
		if !slices.Equal(seen[i], want[i]) {
			t.Errorf("hook call %d saw %v, want %v", i, seen[i], want[i])
		}
	}
}

func TestStopRefreshAwaitsTheInFlightRead(t *testing.T) {
	// The first read commits, the second parks, so the assertions can tell an
	// evicted catalog from an untouched one.
	slow := serving(tool("kubectl_logs"))
	slow.block, slow.passes = make(chan struct{}), 1
	c := newTestCatalog(t, stubSource{"alpha": slow}, fastTuning)
	c.Refresh(t.Context(), "alpha")

	c.Trigger("alpha")
	waitFor(t, func() bool { return slow.reading.Load() })

	stopped := make(chan struct{})
	go func() { c.StopRefresh("alpha"); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopRefresh did not cancel and await the in-flight read, so a disable cannot stop the loop")
	}

	if got, want := ids(c), []string{"mcp__alpha__kubectl_logs"}; !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v: our own cancellation is not a backend failure", got, want)
	}
	if got := c.Errors()["alpha"]; got != "" {
		t.Errorf("recorded error %q for a refresh we cancelled ourselves", got)
	}
}

// --- helpers ---

var errSpawn = errors.New("spawn refused")

type stubSource map[string]lister

func (s stubSource) names() []string { return slices.Sorted(maps.Keys(s)) }

func (s stubSource) lister(name string) (lister, bool) {
	l, ok := s[name]
	return l, ok
}

// noHandshake gives a stub lister the third lister method. Zero means the stub
// never connects, so the catalog's read budget is its list budget alone.
type noHandshake struct{ connect time.Duration }

func (h noHandshake) ConnectTimeout() time.Duration { return h.connect }

type refusingLister struct {
	noHandshake
	t *testing.T
}

func (l refusingLister) ListTools(context.Context) ([]*mcp.Tool, error) {
	l.t.Error("the restarted catalog listed a backend; the point of persistence is answering before that")
	return nil, nil
}
func (l refusingLister) Generation() uint64 { return 1 }

// fakeLister is one configurable stand-in for a backend: it serves a tool set or
// an error, either of which a test can swap mid-run, optionally parks every read
// after the first passes of them, and optionally runs a hook inside each read.
type fakeLister struct {
	noHandshake
	passes  int64
	block   chan struct{}
	onRead  func()
	calls   atomic.Int64
	reading atomic.Bool

	mu    sync.Mutex
	tools []*mcp.Tool
	err   error
}

func serving(tools ...*mcp.Tool) *fakeLister { return &fakeLister{tools: tools} }

func failing(err error) *fakeLister { return &fakeLister{err: err} }

func (l *fakeLister) set(err error, tools ...*mcp.Tool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tools, l.err = tools, err
}

func (l *fakeLister) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	reads := l.calls.Add(1)
	if l.onRead != nil {
		l.onRead()
	}
	if l.block != nil && reads > l.passes {
		l.reading.Store(true)
		defer l.reading.Store(false)
		select {
		case <-l.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	return l.tools, nil
}

func (l *fakeLister) Generation() uint64 { return 1 }

// scriptedLister serves a canned sequence of reads and generations, so a test
// can move the generation between the sample taken after a read and the check
// taken at commit without depending on scheduling.
type scriptedLister struct {
	noHandshake
	mu    sync.Mutex
	reads [][]*mcp.Tool
	gens  []uint64
	read  int
	gen   int
}

func (l *scriptedLister) ListTools(context.Context) ([]*mcp.Tool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tools := l.reads[min(l.read, len(l.reads)-1)]
	l.read++
	return tools, nil
}

func (l *scriptedLister) Generation() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	g := l.gens[min(l.gen, len(l.gens)-1)]
	l.gen++
	return g
}

// sessionLister drives a testfake over a real client session, so coalescing is
// asserted on the fake's tools/list counter rather than on a stub's bookkeeping.
type sessionLister struct {
	noHandshake
	sess *mcp.ClientSession
}

func (l *sessionLister) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	res, err := l.sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

func (l *sessionLister) Generation() uint64 { return 1 }

func listerFor(t *testing.T, f *testfake.Fake) *sessionLister {
	t.Helper()
	tr, err := f.Transport(context.Background())
	if err != nil {
		t.Fatalf("fake transport: %v", err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "catalog-test", Version: "test"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect to fake: %v", err)
	}
	t.Cleanup(func() {
		sess.Close()
		f.Close()
	})
	return &sessionLister{sess: sess}
}

// listGate parks each of the first holds tools/list responses until released,
// after the handler has captured the tool set. testfake's BeforeList runs before
// the handler, so a set changed while a read is parked would leak into that read
// and a test could not tell which read's result was committed.
type listGate struct{ gates []chan struct{} }

func holdLists(t *testing.T, f *testfake.Fake, holds int) *listGate {
	t.Helper()
	g := &listGate{}
	for range holds {
		g.gates = append(g.gates, make(chan struct{}))
	}
	f.Server().AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/list" {
				if n := int(f.ListCalls.Load()); n >= 1 && n <= len(g.gates) {
					<-g.gates[n-1]
				}
			}
			return res, err
		}
	})
	t.Cleanup(func() {
		for _, gate := range g.gates {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	})
	return g
}

func (g *listGate) release(n int) { close(g.gates[n-1]) }

func newTestCatalog(t *testing.T, src backends, tune tuning) *Catalog {
	t.Helper()
	c := newCatalog(src, filepath.Join(t.TempDir(), "catalog.json"), tune)
	t.Cleanup(func() { stopAll(c, src.names()) })
	return c
}

// stopAll leaves no refresh loop running past the end of a test, which would
// otherwise read a closed session or write to a removed temporary directory.
func stopAll(c *Catalog, servers []string) {
	for _, name := range servers {
		c.StopRefresh(name)
	}
}

// httpRegistry serves each fake over streamable HTTP so the catalog runs against
// a real *backend.Registry. Stateless keeps the client from holding a standalone
// SSE stream open, which would block the test server's shutdown.
func httpRegistry(t *testing.T, fakes ...*testfake.Fake) *backend.Registry {
	t.Helper()
	cfg := &config.Config{Backends: make(map[string]config.Backend, len(fakes))}
	for _, f := range fakes {
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return f.Server() },
			&mcp.StreamableHTTPOptions{Stateless: true},
		))
		t.Cleanup(func() {
			srv.CloseClientConnections()
			srv.Close()
			f.Close()
		})
		cfg.Backends[f.Name] = config.Backend{Name: f.Name, HTTPURL: srv.URL, TimeoutSec: 10}
	}
	ov, err := backend.LoadOverrides(filepath.Join(t.TempDir(), "overrides.json"))
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	return backend.NewRegistry(cfg, ov, backend.Hooks{})
}

func ids(c *Catalog) []string {
	out := make([]string, 0)
	for _, e := range c.Entries() {
		out = append(out, e.ID)
	}
	return out
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: name + " description",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pod":{"type":"string"}}}`),
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
