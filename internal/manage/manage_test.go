package manage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahodges/mcpd/internal/backend"
	"github.com/ahodges/mcpd/internal/config"
)

type fakeCatalog struct {
	mu        sync.Mutex
	triggered []string
	dropped   []string
}

func (f *fakeCatalog) Trigger(server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggered = append(f.triggered, server)
}

func (f *fakeCatalog) Drop(server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, server)
}

func (f *fakeCatalog) saw(list []string, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

type fakeTokens struct {
	mu        sync.Mutex
	forgot    []string
	held      map[string]bool
	forgetErr error
	// publishedAt reports whether a name is already routable, so Forget can pin the
	// ordering rather than only the end state: a deletion that runs after publication
	// leaves the same state behind but allows a dial to authenticate in between.
	publishedAt   func(string) bool
	forgotWhileUp []string
}

func (f *fakeTokens) Forget(server string) error {
	up := f.publishedAt != nil && f.publishedAt(server)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, server)
	if up {
		f.forgotWhileUp = append(f.forgotWhileUp, server)
	}
	if f.forgetErr != nil {
		return f.forgetErr
	}
	delete(f.held, server)
	return nil
}

func (f *fakeTokens) Reconcile(map[string]config.Identity) error { return nil }

func (f *fakeTokens) has(server string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[server]
}

type harness struct {
	m      *Manager
	writer *config.Writer
	reg    *backend.Registry
	ov     *backend.Overrides
	cat    *fakeCatalog
	tok    *fakeTokens
	path   string
}

func newHarness(t *testing.T, body string) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	w, err := config.NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(dir, "overrides.json"))
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	h := &harness{
		writer: w,
		reg:    backend.NewRegistry(cfg, ov, backend.Hooks{}),
		ov:     ov,
		cat:    &fakeCatalog{},
		tok:    &fakeTokens{held: map[string]bool{}},
		path:   path,
	}
	h.m = New(w, h.reg, h.cat, ov, h.tok)
	h.tok.publishedAt = func(name string) bool { _, ok := h.reg.Get(name); return ok }
	return h
}

func (h *harness) declared(t *testing.T) map[string]config.Backend {
	t.Helper()
	cfg, err := config.Load(h.path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg.Backends
}

// Scenario: "An added backend serves tools without a restart", at the operation level: the
// declaration is written and the backend is published and refreshed.
func TestAddDeclaresPublishesAndRefreshes(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)

	if _, err := h.m.Add("flint", config.Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, ok := h.declared(t)["flint"]; !ok {
		t.Error("the declaration was not written")
	}
	if _, ok := h.reg.Get("flint"); !ok {
		t.Error("the backend was not published")
	}
	if !h.cat.saw(h.cat.triggered, "flint") {
		t.Error("no catalog refresh was triggered for the added backend")
	}
}

// Scenario: "A rejected declaration changes nothing", including every piece of name-keyed
// state. This is the case an earlier draft got wrong by deleting state before the commit.
func TestARejectedAddLeavesStateUntouched(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)
	if err := h.reg.Disable("art"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	h.tok.held["art"] = true

	if _, err := h.m.Add("art", config.Backend{Command: "y"}); !errors.Is(err, config.ErrDuplicate) {
		t.Fatalf("Add of an existing name = %v, want ErrDuplicate", err)
	}

	if !h.ov.Disabled("art") {
		t.Error("a rejected add deleted a live backend's override")
	}
	if !h.tok.has("art") {
		t.Error("a rejected add deleted a live backend's stored token")
	}
	if got := h.declared(t)["art"].Command; got != "x" {
		t.Errorf("art command = %q, want the original: a rejected add rewrote the declaration", got)
	}
}

// The hygiene deletion has to run before publication, because a published backend is
// immediately routable and a dial that beat the deletion could authenticate with the
// record a previous removal was supposed to have deleted.
func TestAddClearsStaleStateBeforePublishing(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)
	// What a removal whose cleanup failed leaves behind.
	if err := h.ov.Rebind("flint", config.Identity{}); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	h.tok.held["flint"] = true

	if _, err := h.m.Add("flint", config.Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if h.tok.has("flint") {
		t.Error("a stale stored token survived the add")
	}
	if h.ov.Disabled("flint") {
		t.Error("a stale disabled entry survived the add")
	}
	if got := h.reg.Health()["flint"].State; got == backend.StateDisabled {
		t.Errorf("flint state = %q, want enabled", got)
	}
	h.tok.mu.Lock()
	defer h.tok.mu.Unlock()
	if len(h.tok.forgotWhileUp) != 0 {
		t.Errorf("the stale token was deleted after the backend was published (%v): a dial could have used it first", h.tok.forgotWhileUp)
	}
}

// Scenario: "A removed backend stops serving", plus the state cleanup that goes with it.
func TestRemoveUndeclaresTearsDownAndCleansUp(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}, "flint": {"command": "y"}}}`)
	if err := h.reg.Disable("flint"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	h.tok.held["flint"] = true

	if _, err := h.m.Remove("flint"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok := h.declared(t)["flint"]; ok {
		t.Error("the declaration was not removed")
	}
	if _, ok := h.reg.Get("flint"); ok {
		t.Error("the backend is still routable")
	}
	if h.writer.Declared("flint") {
		t.Error("the declared set still contains the removed backend")
	}
	if !h.cat.saw(h.cat.dropped, "flint") {
		t.Error("the catalog was not told to drop the removed backend's tools")
	}
	if h.ov.Disabled("flint") || h.tok.has("flint") {
		t.Error("state survived under a name that is no longer declared")
	}
}

// A cleanup failure is reported but does not stop the removal: the declaration is already
// gone and the backend is already down, so aborting would only add inconsistency.
func TestRemoveReportsCleanupFailureWithoutAborting(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}, "flint": {"command": "y"}}}`)
	h.tok.forgetErr = errors.New("disk on fire")

	warnings, err := h.m.Remove("flint")
	if err != nil {
		t.Fatalf("Remove returned an error for a post-commit cleanup failure: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("the cleanup failure was not reported")
	}
	if _, ok := h.reg.Get("flint"); ok {
		t.Error("the backend was not removed")
	}
}

// Scenario: "A malformed file changes nothing at all, and every existing backend keeps its
// session".
func TestReloadIsAllOrNothing(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)

	if err := os.WriteFile(h.path, []byte(`{"backends": {"art": {"command": "x"}, "broken": {}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err == nil {
		t.Fatal("Reload accepted a file with an invalid declaration")
	}
	if _, ok := h.reg.Get("art"); !ok {
		t.Error("a rejected reload tore down an existing backend")
	}
	if _, ok := h.reg.Get("broken"); ok {
		t.Error("a rejected reload published a backend from the invalid file")
	}
}

// Scenario: "A hand-added backend appears on reload" and "A hand-removed one is torn down".
// The refresh matters as much as the registration: without it a hand-added backend is
// published with no tools until the next TTL tick.
func TestReloadAppliesHandEdits(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}, "gone": {"command": "y"}}}`)
	h.tok.held["gone"] = true

	body := `{"backends": {"art": {"command": "x"}, "fresh": {"command": "z"}}}`
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, ok := h.reg.Get("fresh"); !ok {
		t.Error("a hand-added backend was not registered")
	}
	if !h.cat.saw(h.cat.triggered, "fresh") {
		t.Error("no catalog refresh was triggered for a hand-added backend")
	}
	if _, ok := h.reg.Get("gone"); ok {
		t.Error("a hand-removed backend was not torn down")
	}
	if h.tok.has("gone") {
		t.Error("a hand-removed backend kept its stored token")
	}
}

// Scenario: "A disabled backend whose declaration changed is still disabled afterwards",
// and it must hold across a restart too: carrying the state into the replacement object
// alone leaves the persisted entry recording the previous declaration.
func TestReloadKeepsADisabledBackendDisabledAcrossARestart(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x", "args": ["one"]}}}`)
	if err := h.reg.Disable("art"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	body := `{"backends": {"art": {"command": "x", "args": ["two"]}}}`
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := h.reg.Health()["art"].State; got != backend.StateDisabled {
		t.Errorf("state after a declaration edit = %q, want %q", got, backend.StateDisabled)
	}
	// The restart: a fresh registry over the same declarations and the same state.
	cfg, err := config.Load(h.path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ov, err := backend.LoadOverrides(filepath.Join(filepath.Dir(h.path), "overrides.json"))
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	if err := ov.Reconcile(map[string]config.Identity{"art": config.IdentityOf(cfg.Backends["art"])}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	restarted := backend.NewRegistry(cfg, ov, backend.Hooks{})
	if got := restarted.Health()["art"].State; got != backend.StateDisabled {
		t.Errorf("state after a restart = %q, want %q: the override was not rebound to the edited declaration", got, backend.StateDisabled)
	}
}

// An unchanged backend must not be rebuilt: doing so would drop a live session, terminate a
// healthy child, and force an OAuth backend through a handshake it did not need.
func TestReloadLeavesAnUnchangedBackendAlone(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}, "flint": {"command": "y"}}}`)
	before, _ := h.reg.Get("art")

	body := `{"backends": {"art": {"command": "x"}, "flint": {"command": "changed"}}}`
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after, _ := h.reg.Get("art")
	if before != after {
		t.Error("an unchanged backend was rebuilt, dropping its session")
	}
	if h.cat.saw(h.cat.triggered, "art") {
		t.Error("an unchanged backend was refreshed")
	}
}

// A repointed declaration loses its stored grant. This is hygiene layered on the store's own
// identity binding, so it is asserted at the operation level rather than trusted to it.
func TestReloadForgetsAGrantWhenTheIdentityChanges(t *testing.T) {
	h := newHarness(t, `{"backends": {"api": {"http_url": "https://one.test/mcp", "auth": "oauth"}}}`)
	h.tok.held["api"] = true

	body := `{"backends": {"api": {"http_url": "https://two.test/mcp", "auth": "oauth"}}}`
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if h.tok.has("api") {
		t.Error("a repointed backend kept the grant issued for its previous endpoint")
	}
}

// A change that does not touch the identity keeps the grant, which is the guard against the
// rule being so eager that every edit forces a re-authorization.
func TestReloadKeepsAGrantWhenOnlyAnUnrelatedFieldChanges(t *testing.T) {
	h := newHarness(t, `{"backends": {"api": {"http_url": "https://one.test/mcp", "auth": "oauth"}}}`)
	h.tok.held["api"] = true

	body := `{"backends": {"api": {"http_url": "https://one.test/mcp", "auth": "oauth", "timeout": 180}}}`
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !h.tok.has("api") {
		t.Error("an unrelated declaration change forced a re-authorization")
	}
}

// Every operation checks the shutdown latch before it mutates anything, reload included:
// reload registers backends exactly as an add does, so one queued behind shutdown would
// otherwise publish them and trigger a refresh that spawns a child after the walk.
func TestEveryOperationRefusesAfterShutdown(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)
	before, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	h.reg.Shutdown()

	if _, err := h.m.Add("flint", config.Backend{Command: "npx"}); !errors.Is(err, backend.ErrRegistryShutdown) {
		t.Errorf("Add after shutdown = %v, want ErrRegistryShutdown", err)
	}
	if _, err := h.m.Remove("art"); !errors.Is(err, backend.ErrRegistryShutdown) {
		t.Errorf("Remove after shutdown = %v, want ErrRegistryShutdown", err)
	}
	if _, err := h.m.Reload(); !errors.Is(err, backend.ErrRegistryShutdown) {
		t.Errorf("Reload after shutdown = %v, want ErrRegistryShutdown", err)
	}
	after, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("an operation refused after shutdown still wrote the declaration file")
	}
}

// Scenario: "A concurrent removal and add of one name stay consistent". Serializing only
// the declaration write is not enough: the removal's cleanup would then delete the state of
// the backend the add had just published, leaving a declaration with nothing behind it.
//
// The add is driven from inside the window the lock is held across, rather than from a
// racing goroutine, because a race that only sometimes interleaves proves nothing about
// whether the lock is doing any work.
func TestARemovalAndAnAddOfOneNameStayConsistent(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}, "flint": {"command": "y"}}}`)
	h.tok.held["flint"] = true

	var wg sync.WaitGroup
	h.m.afterCommit = func(op, name string) {
		if op != "remove" || name != "flint" {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.m.Add("flint", config.Backend{Command: "readded"})
		}()
		// Long enough that an unserialized add gets all the way through its own commit
		// and publication before this removal's cleanup runs.
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := h.m.Remove("flint"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wg.Wait()

	_, declared := h.declared(t)["flint"]
	_, published := h.reg.Get("flint")
	if declared != published {
		t.Errorf("declared=%v published=%v: the declaration and the runtime disagree", declared, published)
	}
	if declared && h.tok.has("flint") {
		t.Error("the re-added backend inherited the removed backend's stored token")
	}
}

// Reconcile is the backstop for a crash between a commit and its cleanup. The two stores
// resolve a mismatch in opposite directions, and asserting both here is what stops one of
// them being "simplified" to match the other.
func TestReconcileResolvesEachStoreItsOwnWay(t *testing.T) {
	h := newHarness(t, `{"backends": {"art": {"command": "x"}}}`)
	// A disable recorded under a declaration that no longer matches, plus state under a
	// name that is not declared at all.
	if err := h.ov.Rebind("art", config.Identity{}); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if err := h.reg.Disable("art"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := h.ov.Rebind("art", config.Identity{Resource: "https://stale.test"}); err != nil {
		t.Fatalf("Rebind: %v", err)
	}

	cfg, err := config.Load(h.path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := h.m.Reconcile(cfg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !h.ov.Disabled("art") {
		t.Error("reconcile discarded a disable whose identity had drifted, which would start a process the user stopped")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(h.path), "overrides.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "stale.test") {
		t.Error("the rebound entry still records the old declaration identity")
	}
}
