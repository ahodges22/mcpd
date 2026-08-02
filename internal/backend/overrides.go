package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/ahodges22/mcpd/internal/atomicfile"
	"github.com/ahodges22/mcpd/internal/config"
)

// Overrides records which backends the user has disabled, and which declaration each
// disable was aimed at. It lives under the daemon's state directory, separately from
// the declarations, so a runtime toggle never rewrites a declaration.
type Overrides struct {
	path string

	mu           sync.Mutex
	disabled     map[string]config.Identity
	declarations declarationGuard
}

type declarationGuard interface {
	HoldDeclared(name string, want *config.Identity, fn func()) bool
}

type overrideDocument struct {
	// Disabled is a map of name to the declaration identity the disable was recorded
	// under. It was an array of names before identities existed, and LoadOverrides
	// still accepts that shape: rejecting it would silently enable every backend the
	// user had disabled on the first start after the upgrade.
	Disabled json.RawMessage `json:"disabled"`
}

// LoadOverrides reads the override file. An absent file is a first run, not an
// error.
func LoadOverrides(path string, declarations declarationGuard) (*Overrides, error) {
	if declarations == nil {
		return nil, errors.New("declaration guard is required")
	}
	o := &Overrides{path: path, disabled: make(map[string]config.Identity), declarations: declarations}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	var doc overrideDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse overrides: %w", err)
	}
	if len(doc.Disabled) == 0 {
		return o, nil
	}
	if err := json.Unmarshal(doc.Disabled, &o.disabled); err == nil {
		return o, nil
	}
	var legacy []string
	if err := json.Unmarshal(doc.Disabled, &legacy); err != nil {
		return nil, fmt.Errorf("parse overrides: %w", err)
	}
	// A legacy entry records no identity. The zero value is what Reconcile treats as
	// "written before identities existed", and it rebinds rather than discarding.
	for _, name := range legacy {
		o.disabled[name] = config.Identity{}
	}
	return o, nil
}

// Reconcile settles the override file against the current declarations. An entry for a
// name that is not declared is deleted, because state left under an undeclared name
// would silently apply to a later backend that reused it.
//
// An entry whose identity does not match, or which records none at all, is rebound to
// the current declaration and kept. That is the opposite of what the token store does
// with a mismatch, and the asymmetry is deliberate: a mismatch cannot distinguish a
// stale entry from a repointed declaration without a per-declaration generation
// counter, so each store fails toward its own safe answer. Discarding a token means
// refusing to use it; discarding a disable means starting a process the user stopped.
func (o *Overrides) Reconcile(declared map[string]config.Identity) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	next := make(map[string]config.Identity, len(o.disabled))
	changed := false
	for name, recorded := range o.disabled {
		current, ok := declared[name]
		if !ok {
			changed = true
			continue
		}
		if recorded != current {
			changed = true
		}
		next[name] = current
	}
	if !changed {
		return nil
	}
	if err := o.save(next); err != nil {
		return err
	}
	o.disabled = next
	return nil
}

func (o *Overrides) Disabled(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.disabled[name]
	return ok
}

// set persists name's new state before recording it in memory, so what the
// daemon believes can never be ahead of what a restart would read.
//
// The write happens under the declaration guard, so the whole
// check-and-replace is atomic against a concurrent removal: a write that observed
// the name as declared has landed before that removal's cleanup runs, and one
// arriving later is refused. Refusing loses nothing, because an undeclared backend
// has no state worth keeping.
func (o *Overrides) set(name string, disabled bool, id config.Identity) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	next := maps.Clone(o.disabled)
	if disabled {
		next[name] = id
	} else {
		delete(next, name)
	}
	write := func() error {
		if err := o.save(next); err != nil {
			return err
		}
		o.disabled = next
		return nil
	}
	var err error
	if !o.declarations.HoldDeclared(name, nil, func() { err = write() }) {
		return fmt.Errorf("%s: %w", name, ErrUndeclared)
	}
	return err
}

// ErrUndeclared reports that state was not persisted because its backend is no longer
// declared.
var ErrUndeclared = errors.New("backend no longer declared")

func (o *Overrides) save(disabled map[string]config.Identity) error {
	if disabled == nil {
		disabled = map[string]config.Identity{}
	}
	raw, err := json.Marshal(struct {
		Disabled map[string]config.Identity `json:"disabled"`
	}{disabled})
	if err != nil {
		return fmt.Errorf("marshal overrides: %w", err)
	}
	dir := filepath.Dir(o.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := atomicfile.Write(o.path, raw, 0o600); err != nil {
		return fmt.Errorf("write overrides: %w", err)
	}
	return nil
}

// Forget deletes name's entry outright. A removal calls it, because state left under a
// name that is no longer declared would silently apply to a later backend that reused it.
func (o *Overrides) Forget(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.disabled[name]; !ok {
		return nil
	}
	next := maps.Clone(o.disabled)
	delete(next, name)
	if err := o.save(next); err != nil {
		return err
	}
	o.disabled = next
	return nil
}

// Rebind records name's existing disable under a new declaration identity, which is what a
// reload replacement does so the disable survives a restart under the edited declaration.
func (o *Overrides) Rebind(name string, id config.Identity) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.disabled[name]; !ok {
		return nil
	}
	next := maps.Clone(o.disabled)
	next[name] = id
	if err := o.save(next); err != nil {
		return err
	}
	o.disabled = next
	return nil
}
