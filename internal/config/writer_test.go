package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newWriterAt(t *testing.T, body string) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	w, _, err := NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w, path
}

func TestWriterInitialSnapshotMatchesDeclaredState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"backends":{"platform":{"http_url":"https://first.example/mcp","auth":"oauth"}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, initial, err := NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"backends":{"platform":{"http_url":"https://second.example/mcp","auth":"oauth"}}}`), 0o600); err != nil {
		t.Fatalf("replace config: %v", err)
	}

	if got := initial.Backends["platform"].HTTPURL; got != "https://first.example/mcp" {
		t.Errorf("initial config URL = %q, want first snapshot", got)
	}
	id, ok := w.Identity("platform")
	if !ok || id.Resource != "https://first.example/mcp" {
		t.Errorf("initial declared identity = %+v, %v; want first snapshot", id, ok)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(raw)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// Scenario: "A write preserves what the daemon does not model". The daemon rewrites only
// the entry it changed, so a field its own types have never heard of survives.
func TestWriter_PreservesUnmodelledFields(t *testing.T) {
	w, path := newWriterAt(t, `{
  "backends": {
    "platform": {"command": "platform-mcp-server", "future_knob": {"deep": [1, 2]}}
  },
  "top_level_extra": "keep me"
}`)

	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := readFile(t, path)
	if !strings.Contains(got, "future_knob") || !strings.Contains(got, `"deep"`) {
		t.Errorf("unmodelled backend field dropped:\n%s", got)
	}
	if !strings.Contains(got, "top_level_extra") {
		t.Errorf("unmodelled top-level field dropped:\n%s", got)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Add: %v", err)
	}
	if _, ok := cfg.Backends["flint"]; !ok {
		t.Error("added backend missing after reload")
	}
}

// Scenario: "A write is refused when the file changed underneath it", and the file is left
// byte identical.
func TestWriter_RefusesStaleWriteAndChangesNothing(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	handEdit := `{"backends": {"platform": {"command": "x"}, "byhand": {"command": "y"}}}`
	if err := os.WriteFile(path, []byte(handEdit), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := w.Add("flint", Backend{Command: "npx"})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Add error = %v, want ErrStale", err)
	}
	if got := readFile(t, path); got != handEdit {
		t.Errorf("refused write still changed the file:\n%s", got)
	}
}

// Scenario: "A second write is not refused", which is what proves the baseline advanced
// rather than staying pinned to the bytes read at startup.
func TestWriter_BaselineAdvancesAfterSuccess(t *testing.T) {
	w, _ := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if _, err := w.Add("github", Backend{HTTPURL: "https://example.test/mcp"}); err != nil {
		t.Fatalf("second Add: %v", err)
	}
}

// Scenario: "An edit landing after the comparison is archived rather than lost", and it
// survives ten later routine writes. A copy taken before the exchange would miss it.
func TestWriter_ArchivesAnEditThatLandsAfterTheComparison(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	sneaky := `{"backends": {"platform": {"command": "x"}, "sneaky": {"command": "z"}}}`
	w.beforeExchange = func() {
		if err := os.WriteFile(path, []byte(sneaky), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	w.beforeExchange = nil

	find := func() string {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if !strings.Contains(e.Name(), "displaced") {
				continue
			}
			if body := readFile(t, filepath.Join(filepath.Dir(path), e.Name())); strings.Contains(body, "sneaky") {
				return e.Name()
			}
		}
		return ""
	}
	name := find()
	if name == "" {
		t.Fatal("the displaced hand edit was not archived")
	}

	for i := range 10 {
		if _, err := w.Add("filler"+string(rune('a'+i)), Backend{Command: "x"}); err != nil {
			t.Fatalf("routine write %d: %v", i, err)
		}
	}
	if find() == "" {
		t.Error("the displaced hand edit was rotated away by routine writes")
	}
}

// Scenario: "Routine archives are bounded". A displacement the daemon itself wrote is
// reconstructible, so those are the ones that may be discarded.
func TestWriter_BoundsRoutineArchives(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	for i := range routineArchiveLimit + 3 {
		if _, err := w.Add("b"+string(rune('a'+i)), Backend{Command: "x"}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	routine := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), routineArchiveInfix) {
			routine++
		}
	}
	if routine > routineArchiveLimit {
		t.Errorf("kept %d routine archives, want at most %d", routine, routineArchiveLimit)
	}
}

// Scenario: "An unsupported exchange refuses the write". There is deliberately no
// plain-rename fallback: a fallback reopens the window the exchange exists to close, and
// it does so exactly where nobody would notice.
func TestWriter_RefusesWhenExchangeUnavailable(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)
	before := readFile(t, path)
	w.exchange = func(string, string) error { return errors.ErrUnsupported }

	_, err := w.Add("flint", Backend{Command: "npx"})
	if err == nil {
		t.Fatal("Add succeeded with no atomic exchange available")
	}
	if got := readFile(t, path); got != before {
		t.Errorf("refused write still changed the file:\n%s", got)
	}
}

// Scenario: "A failing archive still completes the operation", and the displaced file is
// left recoverable and owner-only. Archiving happens after the commit point, so treating
// it as an error would leave the file committed and the caller believing it failed.
func TestWriter_ArchiveFailureIsAWarningNotAnError(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)
	w.archiveFails = true

	warnings, err := w.Add("flint", Backend{Command: "npx"})
	if err != nil {
		t.Fatalf("Add returned an error for a post-commit archive failure: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("archive failure was not reported as a warning")
	}
	if !strings.Contains(readFile(t, path), "flint") {
		t.Error("the write did not commit")
	}
	left, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"+displacedTempInfix+"*"))
	if err != nil || len(left) == 0 {
		t.Fatalf("displaced content was removed rather than left recoverable (glob=%v err=%v)", left, err)
	}
	if got := mode(t, left[0]); got != 0o600 {
		t.Errorf("displaced file mode = %v, want 0600", got)
	}
}

// Scenario: "A permissive mode is tightened on write", and its harder sibling, "A file
// replaced by an editor mid-write is still not left exposed". Tightening the inode the
// daemon read is not enough, because an editor can swap in a fresh permissive one after
// the check: the mode of the inode checked is not the mode of the inode displaced.
func TestWriter_DisplacedInodeIsRestrictedEvenWhenSwapped(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)
	if got := mode(t, filepath.Dir(path)); got != 0o700 {
		t.Errorf("declaration directory mode = %v, want 0700", got)
	}

	w.beforeExchange = func() {
		swap := filepath.Join(filepath.Dir(path), "swap.json")
		if err := os.WriteFile(swap, []byte(readFile(t, path)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Rename(swap, path); err != nil {
			t.Fatalf("Rename: %v", err)
		}
	}
	w.archiveFails = true
	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	left, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"+displacedTempInfix+"*"))
	if err != nil || len(left) == 0 {
		t.Fatalf("no displaced file left to inspect (glob=%v err=%v)", left, err)
	}
	if got := mode(t, left[0]); got != 0o600 {
		t.Errorf("displaced inode mode = %v, want 0600: an editor swapped in a permissive inode after the check", got)
	}
}

// A configuration symlinked into a dotfiles repository is a normal setup, and an exchange
// performed on the link would swap the link itself for a regular file: the daemon would
// then read a detached copy while the user kept editing the original.
func TestWriter_WritesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-config.json")
	if err := os.WriteFile(target, []byte(`{"backends": {"platform": {"command": "x"}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w, _, err := NewWriter(link)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file, detaching the daemon from the file the user edits")
	}
	if !strings.Contains(readFile(t, target), "flint") {
		t.Error("the write did not reach the symlink target")
	}
}

// Scenario: "A duplicate name is refused rather than replacing an existing declaration",
// and its mirror for removal.
func TestWriter_RefusesDuplicateAndUnknown(t *testing.T) {
	w, _ := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	if _, err := w.Add("platform", Backend{Command: "y"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("Add of an existing name = %v, want ErrDuplicate", err)
	}
	if _, err := w.Remove("nope"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Remove of an undeclared name = %v, want ErrUnknown", err)
	}
	if _, err := w.Add("Bad Name", Backend{Command: "y"}); err == nil {
		t.Error("Add accepted an invalid backend name")
	}
	if _, err := w.Add("both", Backend{Command: "y", HTTPURL: "https://x.test"}); err == nil {
		t.Error("Add accepted a declaration naming both command and http_url")
	}
}

// Scenario: "Removing the last backend leaves a loadable file".
func TestWriter_RemovingTheLastBackendStaysLoadable(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	if _, err := w.Remove("platform"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after removing the last backend: %v", err)
	}
	if len(cfg.Backends) != 0 {
		t.Errorf("backends = %v, want empty", cfg.Backends)
	}
}

// Scenario: "Two concurrent daemon writes do not interleave". One is refused as a
// duplicate rather than as stale, which is what serializing inside the critical section
// buys: the loser re-reads a file that already contains the winner.
func TestWriter_SerializesConcurrentWrites(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.Add("flint", Backend{Command: "npx"})
		}()
	}
	wg.Wait()

	ok, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDuplicate):
			dup++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || dup != 1 {
		t.Errorf("got %d successes and %d duplicate refusals, want exactly one of each", ok, dup)
	}

	var doc struct {
		Backends map[string]json.RawMessage `json:"backends"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := doc.Backends["flint"]; !ok || len(doc.Backends) != 2 {
		t.Errorf("backends = %v, want exactly platform and one flint", doc.Backends)
	}
}

// Reload adopts the bytes it validated as the new baseline. Without that, reload would
// reconcile the runtime while every later write from the surface refused as stale until
// the next restart.
func TestWriter_ReloadAdoptsBaselineSoLaterWritesSucceed(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	handEdit := `{"backends": {"platform": {"command": "x"}, "byhand": {"command": "y"}}}`
	if err := os.WriteFile(path, []byte(handEdit), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := w.Add("flint", Backend{Command: "npx"}); !errors.Is(err, ErrStale) {
		t.Fatalf("Add before reload = %v, want ErrStale", err)
	}

	cfg, err := w.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := cfg.Backends["byhand"]; !ok {
		t.Error("reload did not adopt the hand-added backend")
	}
	if _, err := w.Add("flint", Backend{Command: "npx"}); err != nil {
		t.Errorf("Add after reload = %v, want success: the baseline was not adopted", err)
	}
}

// Scenario: "A malformed file changes nothing at all". Reload validates the whole file
// before it touches anything, which is the level at which a bad hand edit can be stopped
// from taking down a working backend.
func TestWriter_ReloadIsAllOrNothing(t *testing.T) {
	w, path := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	for _, bad := range []string{
		`{"backends": {"platform": {"command": "x"}, "broken": {}}}`,
		`{"backends": {"platform": {"command": "x"}`,
		`{"backends": {"platform": {"command": "x"}, "Bad Name": {"command": "y"}}}`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := w.Reload(); err == nil {
			t.Errorf("Reload accepted an invalid file: %s", bad)
		}
		if !w.Declared("platform") {
			t.Fatalf("a rejected reload dropped the declared set: %s", bad)
		}
	}
}

// The declared-set snapshot is what the override and token writers consult, so it must
// track every committed change rather than only what was read at startup.
func TestWriter_DeclaredSetTracksWrites(t *testing.T) {
	w, _ := newWriterAt(t, `{"backends": {"platform": {"command": "x"}}}`)

	if !w.Declared("platform") || w.Declared("flint") {
		t.Fatal("initial declared set is wrong")
	}
	if _, err := w.Add("flint", Backend{HTTPURL: "https://example.test/mcp", Auth: "oauth"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !w.Declared("flint") {
		t.Error("declared set did not pick up an added backend")
	}
	id, ok := w.Identity("flint")
	if !ok || id.Resource != "https://example.test/mcp" || id.Auth != "oauth" || id.Transport != "http" {
		t.Errorf("Identity(flint) = %+v, %v; want the declared resource, auth and transport", id, ok)
	}
	if _, err := w.Remove("flint"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if w.Declared("flint") {
		t.Error("declared set still contains a removed backend")
	}
}

func TestSetRemotePersistsAndPreservesBackends(t *testing.T) {
	w, _ := newWriterAt(t, `{"backends":{"gh":{"http_url":"https://gh.example/mcp","auth":"oauth"}}}`)
	if _, err := w.SetRemote(Remote{Enabled: true, Addr: ":7421"}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	cfg, err := w.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !cfg.Remote.Enabled || cfg.Remote.Addr != ":7421" {
		t.Fatalf("remote not persisted: %+v", cfg.Remote)
	}
	if _, ok := cfg.Backends["gh"]; !ok {
		t.Fatal("backends clobbered by SetRemote")
	}
	if _, err := w.SetRemote(Remote{Enabled: false, Addr: ":7421"}); err != nil {
		t.Fatalf("SetRemote disable: %v", err)
	}
	cfg, err = w.Reload()
	if err != nil {
		t.Fatalf("Reload after disable: %v", err)
	}
	if cfg.Remote.Enabled {
		t.Fatal("disable not persisted")
	}
}
