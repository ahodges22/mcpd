package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	// ErrStale means the file on disk no longer matches what the daemon last read, so
	// someone else wrote it. Refusing is correct rather than merging: the daemon cannot
	// tell an intentional hand edit from a stale editor buffer about to be saved.
	ErrStale = errors.New("configuration file changed on disk")
	// ErrDuplicate and ErrUnknown let a route answer 409 without matching on text.
	ErrDuplicate = errors.New("backend already declared")
	ErrUnknown   = errors.New("backend not declared")
)

const (
	routineArchiveLimit = 10
	// The two series are distinguished on disk because only one of them may be
	// discarded: a version the daemon itself wrote is reconstructible, and one it
	// displaced without writing is not.
	routineArchiveInfix = ".bak."
	displacedArchiveFix = ".displaced."
	displacedTempInfix  = ".displaced-tmp-"
)

// Identity is the part of a declaration that name-keyed state is bound to. A change to
// any of these fields invalidates a stored token, because the token was issued by one
// provider for one resource and the backend name says nothing about either.
type Identity struct {
	Resource  string `json:"resource"`
	Auth      string `json:"auth"`
	Transport string `json:"transport"`
}

func identityOf(b Backend) Identity {
	transport := "http"
	if b.IsStdio() {
		transport = "stdio"
	}
	return Identity{Resource: b.HTTPURL, Auth: b.Auth, Transport: transport}
}

// Writer owns the daemon's single write path into the declaration file. Every write from
// every route and from reload goes through one mutex, so two of the daemon's own writes
// cannot interleave, and the baseline advances on success so the first write does not
// poison every later one into a permanent false refusal.
type Writer struct {
	path string
	dir  string

	mu       sync.Mutex
	baseline []byte

	declMu   sync.RWMutex
	declared map[string]Identity

	// exchange is the atomic commit. It is a field so a test can make the syscall
	// unavailable, which is the one case that must refuse rather than fall back.
	exchange func(displaced, incoming string) error
	// beforeExchange and archiveFails exist for tests that have to open the windows
	// this type is built to survive; production never sets them.
	beforeExchange func()
	archiveFails   bool
}

// NewWriter resolves the path through symlinks once and keeps the resolved location for
// every later read and write. A configuration symlinked into a dotfiles repository is a
// normal setup, and exchanging on the link would swap the link itself for a regular file,
// leaving the daemon reading a detached copy while the user kept editing the original.
func NewWriter(path string) (*Writer, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	dir := filepath.Dir(resolved)
	// The directory mode is the control for credential confidentiality, not the mode of
	// any individual file: a chmod after the commit point may only warn, so a guarantee
	// resting on one would have to choose between aborting late and keeping a readable
	// credential. Establishing this once at startup avoids that choice.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict config directory: %w", err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	w := &Writer{path: resolved, dir: dir, baseline: raw, exchange: exchangeFiles}
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	w.publish(cfg)
	return w, nil
}

func exchangeFiles(displaced, incoming string) error {
	return unix.Renameat2(unix.AT_FDCWD, displaced, unix.AT_FDCWD, incoming, unix.RENAME_EXCHANGE)
}

// Path is the resolved declaration path, which is what everything else must read.
func (w *Writer) Path() string { return w.path }

func (w *Writer) Declared(name string) bool {
	w.declMu.RLock()
	defer w.declMu.RUnlock()
	_, ok := w.declared[name]
	return ok
}

func (w *Writer) Identity(name string) (Identity, bool) {
	w.declMu.RLock()
	defer w.declMu.RUnlock()
	id, ok := w.declared[name]
	return id, ok
}

// HoldDeclared runs fn while the declared set cannot change, and reports whether name is
// declared with a matching identity. A state writer must hold this across its whole file
// replacement rather than checking and releasing: the guarantee needed is not "was it
// declared a moment ago" but "it was still declared when this write landed, so the cleanup
// that follows will see it".
func (w *Writer) HoldDeclared(name string, want *Identity, fn func()) bool {
	w.declMu.RLock()
	defer w.declMu.RUnlock()
	id, ok := w.declared[name]
	if !ok || (want != nil && id != *want) {
		return false
	}
	fn()
	return true
}

// Undeclare drops name before a removal begins its cleanup, so any state writer that saw
// the name as declared has already finished and its write is visible to that cleanup.
func (w *Writer) Undeclare(name string) {
	w.declMu.Lock()
	defer w.declMu.Unlock()
	delete(w.declared, name)
}

func (w *Writer) publish(cfg *Config) {
	next := make(map[string]Identity, len(cfg.Backends))
	for name, b := range cfg.Backends {
		next[name] = identityOf(b)
	}
	w.declMu.Lock()
	w.declared = next
	w.declMu.Unlock()
}

// Add declares a new backend. Everything that could invalidate the write happens before
// the commit point, so a refusal leaves nothing changed and needs no rollback.
func (w *Writer) Add(name string, b Backend) ([]error, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("backend name %q must match %s", name, nameRef)
	}
	if err := validate(name, b); err != nil {
		return nil, err
	}
	entry, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode backend %q: %w", name, err)
	}
	return w.mutate(func(backends map[string]json.RawMessage) error {
		if _, exists := backends[name]; exists {
			return fmt.Errorf("%q: %w", name, ErrDuplicate)
		}
		backends[name] = entry
		return nil
	})
}

// Remove deletes a declaration. The caller tears the backend down only after this returns,
// because the reverse order would kill a live backend for a write that then gets refused.
func (w *Writer) Remove(name string) ([]error, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("backend name %q must match %s", name, nameRef)
	}
	return w.mutate(func(backends map[string]json.RawMessage) error {
		if _, exists := backends[name]; !exists {
			return fmt.Errorf("%q: %w", name, ErrUnknown)
		}
		delete(backends, name)
		return nil
	})
}

// Reload adopts whatever the file currently holds, or nothing at all. It reads, validates
// and adopts inside one mutex hold, which makes those steps atomic against the daemon's
// own writes; that is all a lock the editor does not take can buy. The result is a
// point-in-time snapshot, and the adopted bytes become the baseline so a later divergence
// is reported to the user rather than displacing their edit.
func (w *Writer) Reload() (*Config, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	raw, err := os.ReadFile(w.path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := parse(raw)
	if err != nil {
		return nil, err
	}
	w.baseline = raw
	w.publish(cfg)
	return cfg, nil
}

func (w *Writer) mutate(apply func(map[string]json.RawMessage) error) ([]error, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	raw, err := os.ReadFile(w.path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if string(raw) != string(w.baseline) {
		return nil, ErrStale
	}

	// The document and its backends stay as raw messages so every entry the daemon did
	// not touch is re-encoded byte for byte, including fields its own types do not model.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	backends := map[string]json.RawMessage{}
	if b, ok := doc["backends"]; ok && string(b) != "null" {
		if err := json.Unmarshal(b, &backends); err != nil {
			return nil, fmt.Errorf("parse backends: %w", err)
		}
	}
	if err := apply(backends); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(backends)
	if err != nil {
		return nil, fmt.Errorf("encode backends: %w", err)
	}
	doc["backends"] = encoded
	next, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	next = append(next, '\n')
	if _, err := parse(next); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(w.dir, ".config-*.json")
	if err != nil {
		return nil, fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(next); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write temporary config: %w", err)
	}

	if w.beforeExchange != nil {
		w.beforeExchange()
	}
	// The exchange is the commit point. Nothing above it mutated anything the user can
	// observe, so a failure here or earlier needs no rollback.
	if err := w.exchange(w.path, tmpName); err != nil {
		return nil, fmt.Errorf("exchange config: %w", err)
	}
	committed = true

	// Past this line every failure is a warning. Aborting would leave the file committed
	// and the caller believing nothing happened, which is the inconsistency the single
	// commit point exists to prevent.
	var warnings []error
	displaced := tmpName
	// The displaced inode is whatever was in place at the instant of the swap, which is
	// not necessarily the inode whose mode was read earlier: an editor can replace the
	// file between the two. Restricting it here covers what actually got displaced.
	if err := os.Chmod(displaced, 0o600); err != nil {
		warnings = append(warnings, fmt.Errorf("restrict displaced config: %w", err))
	}
	w.baseline = next
	cfg, err := parse(next)
	if err == nil {
		w.publish(cfg)
	}
	if err := w.archive(displaced, raw); err != nil {
		kept := filepath.Join(w.dir, displacedTempInfix+filepath.Base(displaced))
		if renameErr := os.Rename(displaced, kept); renameErr != nil {
			kept = displaced
		}
		warnings = append(warnings, fmt.Errorf("archive displaced config (left at %s): %w", filepath.Base(kept), err))
		return warnings, nil
	}
	return warnings, nil
}

// archive keeps the version the write displaced. Retention splits on a question the daemon
// can answer exactly: if the displaced bytes match what it last wrote then it displaced its
// own work and the copy is reconstructible, so those rotate; if they do not, it displaced
// something a human wrote and that copy is the only one, so it is kept without limit.
func (w *Writer) archive(displaced string, expected []byte) error {
	if w.archiveFails {
		return errors.New("archive disabled for test")
	}
	body, err := os.ReadFile(displaced)
	if err != nil {
		return err
	}
	routine := string(body) == string(expected)
	infix := displacedArchiveFix
	if routine {
		infix = routineArchiveInfix
	}
	base := filepath.Base(w.path)
	dest := filepath.Join(w.dir, base+infix+strconv.Itoa(w.nextIndex(base+infix)))
	if err := os.Rename(displaced, dest); err != nil {
		return err
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return err
	}
	if routine {
		w.rotate(base + routineArchiveInfix)
	}
	return nil
}

func (w *Writer) indices(prefix string) []int {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(suffix); err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func (w *Writer) nextIndex(prefix string) int {
	idx := w.indices(prefix)
	if len(idx) == 0 {
		return 1
	}
	return idx[len(idx)-1] + 1
}

func (w *Writer) rotate(prefix string) {
	idx := w.indices(prefix)
	for len(idx) > routineArchiveLimit {
		os.Remove(filepath.Join(w.dir, prefix+strconv.Itoa(idx[0])))
		idx = idx[1:]
	}
}

// parse is Load's validation over bytes already in hand, so a write and a reload cannot
// accept a document that a later start would reject.
func parse(raw []byte) (*Config, error) {
	var doc struct {
		Backends *map[string]Backend `json:"backends"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Backends == nil {
		return nil, fmt.Errorf("config declares no backends object")
	}
	c := Config{Backends: map[string]Backend{}}
	for name, b := range *doc.Backends {
		if !ValidName(name) {
			return nil, fmt.Errorf("backend name %q must match %s", name, nameRef)
		}
		if err := validate(name, b); err != nil {
			return nil, err
		}
		b.Name = name
		c.Backends[name] = b
	}
	return &c, nil
}

func validate(name string, b Backend) error {
	if b.IsStdio() == (b.HTTPURL != "") {
		return fmt.Errorf("backend %q must declare exactly one of command or http_url", name)
	}
	for _, pat := range b.EnvPassthrough {
		if prefix, ok := strings.CutSuffix(pat, "*"); ok && prefix == "" {
			return fmt.Errorf("backend %q env_passthrough %q would grant its entire environment", name, pat)
		}
	}
	return nil
}
