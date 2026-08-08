// Package manage owns the three operations that change which backends exist: add,
// remove and reload. Each spans a declaration write, a registry mutation, a teardown and
// some state cleanup, and this package is where their ordering lives.
package manage

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ahodges22/mcpd/internal/backend"
	"github.com/ahodges22/mcpd/internal/config"
)

// Catalog is the part of the tool catalog these operations drive.
type Catalog interface {
	Trigger(server string)
	Drop(server string)
}

// Tokens is the part of the OAuth store these operations drive. Forget discards a
// backend's cached handler, its live token source and its stored record together:
// deleting the record alone would remove only the copy that was not in use.
type Tokens interface {
	Forget(server string) error
	Reconcile(declared map[string]config.Identity) error
}

type SecretIndex interface {
	UpdateBackend(string, *config.Backend)
	Reindex(*config.Config)
}

// Manager serializes add, remove and reload against each other.
//
// The lock is held for the whole sequence rather than only the declaration write. Ending
// it at the commit point is a real race: a removal commits, a concurrent add of the same
// name commits and registers, and the removal's cleanup then deletes the new backend's
// registry entry, its tools, its override and its stored token, leaving a declaration
// with nothing behind it.
//
// The cost is that a teardown can block on in-flight work while another operation waits,
// so a concurrent request can time out at the HTTP layer. These are human clicks, and the
// alternative is generation-tagged cleanup, which is more machinery guarding a race that
// serializing removes outright.
type Manager struct {
	mu      sync.Mutex
	writer  *config.Writer
	reg     *backend.Registry
	cat     Catalog
	ov      *backend.Overrides
	tokens  Tokens
	secrets SecretIndex

	// afterCommit runs between a removal's declaration write and its cleanup. It exists
	// so a test can drive the exact interleaving the operation lock is held across;
	// production never sets it.
	afterCommit func(op, name string)

	// ReloadRemote, when set, runs after a reload adopts the file, so the LAN
	// relogin listener follows a hand-edited declaration instead of serving on
	// with the state the file no longer describes. It reads the committed
	// declaration itself, which is what keeps a toggle that lands mid-reload
	// from being overwritten by the reload's older snapshot.
	ReloadRemote func()
}

func New(w *config.Writer, reg *backend.Registry, cat Catalog, ov *backend.Overrides, tokens Tokens) *Manager {
	return &Manager{writer: w, reg: reg, cat: cat, ov: ov, tokens: tokens}
}

func (m *Manager) WithSecretIndex(index SecretIndex) *Manager {
	m.secrets = index
	return m
}

// Add declares a backend and publishes it.
//
// The shutdown latch is checked before the write rather than at registration: refusing
// after the commit would leave the declaration written and the registry without it, and
// registration is deliberately infallible so it cannot strand a committed write.
func (m *Manager) Add(name string, spec config.Backend) ([]error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reg.ShuttingDown() {
		return nil, fmt.Errorf("add %s: %w", name, backend.ErrRegistryShutdown)
	}

	warnings, err := m.writer.Add(name, spec)
	if err != nil {
		return nil, err
	}

	// Hygiene before publication, not after: a registered backend is immediately
	// routable, and a dial that beat this deletion could authenticate with the very
	// record a previous removal was supposed to have deleted. It does not gate
	// publication, because the declaration is already committed.
	warnings = append(warnings, m.forget(name)...)

	m.reg.Add(name, spec, true)
	if m.secrets != nil {
		m.secrets.UpdateBackend(name, &spec)
	}
	// A trigger, not a gate. A declared backend that cannot be reached is a normal
	// state, shown as down with its cause, so a dial is never a precondition here.
	m.cat.Trigger(name)
	return warnings, nil
}

// Remove deletes a declaration, then tears the backend down. The order matters: tearing
// down first would kill a live backend for a write that then gets refused.
func (m *Manager) Remove(name string) ([]error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reg.ShuttingDown() {
		return nil, fmt.Errorf("remove %s: %w", name, backend.ErrRegistryShutdown)
	}

	warnings, err := m.writer.Remove(name)
	if err != nil {
		return nil, err
	}

	// Dropping the name before the cleanup is what makes a racing state write safe: one
	// that saw the name as declared has already landed, so this cleanup sees it, and one
	// arriving later is refused.
	if m.afterCommit != nil {
		m.afterCommit("remove", name)
	}
	m.writer.Undeclare(name)
	m.reg.Remove(name)
	if m.secrets != nil {
		m.secrets.UpdateBackend(name, nil)
	}
	m.cat.Drop(name)
	return append(warnings, m.forget(name)...), nil
}

// Reload adopts whatever the declaration file currently holds, or nothing at all.
func (m *Manager) Reload() ([]error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reg.ShuttingDown() {
		return nil, fmt.Errorf("reload: %w", backend.ErrRegistryShutdown)
	}

	// The identity a backend is currently declared under, captured before the reload
	// overwrites the snapshot: what decides whether a stored grant survives is the
	// difference between the old declaration and the new one.
	was := make(map[string]config.Identity, len(m.reg.Names()))
	for _, name := range m.reg.Names() {
		if id, ok := m.writer.Identity(name); ok {
			was[name] = id
		}
	}

	// Validated and adopted as one step inside the writer, so a malformed file changes
	// nothing and every existing backend keeps its session.
	cfg, err := m.writer.Reload()
	if err != nil {
		return nil, err
	}

	var warnings []error
	for _, name := range m.reg.Names() {
		if _, still := cfg.Backends[name]; still {
			continue
		}
		// A name that has disappeared is a removal, state deletion included.
		m.writer.Undeclare(name)
		m.reg.Remove(name)
		m.cat.Drop(name)
		warnings = append(warnings, m.forget(name)...)
	}

	for name, spec := range cfg.Backends {
		b, known := m.reg.Get(name)
		if !known {
			m.reg.Add(name, spec, true)
			m.cat.Trigger(name)
			continue
		}
		if config.SameBackend(b.Spec(), spec) {
			// Untouched, so its session, its child and its authorization all survive.
			continue
		}
		id := config.IdentityOf(spec)
		if wasEnabled := m.reg.Replace(name, spec); !wasEnabled {
			// Carrying the state into the replacement fixes only the running daemon: the
			// persisted entry still records the previous declaration, and rebinding it is
			// what keeps the disable across a restart.
			if err := m.ov.Rebind(name, id); err != nil {
				warnings = append(warnings, err)
			}
		}
		if old, ok := was[name]; !ok || old != id {
			if err := m.tokens.Forget(name); err != nil {
				warnings = append(warnings, err)
			}
		}
		m.cat.Trigger(name)
	}
	if m.secrets != nil {
		m.secrets.Reindex(cfg)
	}
	if m.ReloadRemote != nil {
		m.ReloadRemote()
	}
	return warnings, nil
}

// Reconcile settles both state stores against the declarations, which is the backstop for
// a crash between a commit and its cleanup, and the only thing that catches a declaration
// removed or repointed by hand while the daemon was not running.
//
// The two stores resolve a mismatch in opposite directions, deliberately: a token is
// discarded, and a disable is rebound and kept. See backend.Overrides.Reconcile.
func (m *Manager) Reconcile(cfg *config.Config) error {
	declared := make(map[string]config.Identity, len(cfg.Backends))
	for name, spec := range cfg.Backends {
		declared[name] = config.IdentityOf(spec)
	}
	if err := m.ov.Reconcile(declared); err != nil {
		return err
	}
	return m.tokens.Reconcile(declared)
}

// forget deletes a backend's name-keyed state. It is idempotent and does not abort on the
// first failure: the declaration is already gone and the backend is already down, so a
// failure here is reported while the rest still runs. What makes that safe rather than
// merely reported is that neither store honours state it cannot tie to the current
// declaration.
func (m *Manager) forget(name string) []error {
	var warnings []error
	if err := m.ov.Forget(name); err != nil {
		warnings = append(warnings, err)
	}
	if err := m.tokens.Forget(name); err != nil && !errors.Is(err, backend.ErrUndeclared) {
		warnings = append(warnings, err)
	}
	return warnings
}
