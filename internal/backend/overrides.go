package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Overrides records which backends the user has disabled. It lives under the
// daemon's state directory: backends are declared in a file the daemon only ever
// reads, so there is no write path into a file the user hand-edits.
type Overrides struct {
	path string

	mu       sync.Mutex
	disabled map[string]bool
}

type overrideDocument struct {
	Disabled []string `json:"disabled"`
}

// LoadOverrides reads the override file. An absent file is a first run, not an
// error.
func LoadOverrides(path string) (*Overrides, error) {
	o := &Overrides{path: path, disabled: make(map[string]bool)}
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
	for _, name := range doc.Disabled {
		o.disabled[name] = true
	}
	return o, nil
}

func (o *Overrides) Disabled(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.disabled[name]
}

// set persists name's new state before recording it in memory, so what the
// daemon believes can never be ahead of what a restart would read.
func (o *Overrides) set(name string, disabled bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	next := maps.Clone(o.disabled)
	if disabled {
		next[name] = true
	} else {
		delete(next, name)
	}
	if err := o.save(next); err != nil {
		return err
	}
	o.disabled = next
	return nil
}

func (o *Overrides) save(disabled map[string]bool) error {
	raw, err := json.Marshal(overrideDocument{Disabled: slices.Sorted(maps.Keys(disabled))})
	if err != nil {
		return fmt.Errorf("marshal overrides: %w", err)
	}
	dir := filepath.Dir(o.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".overrides-*")
	if err != nil {
		return fmt.Errorf("create temporary overrides: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write overrides: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write overrides: %w", err)
	}
	if err := os.Rename(tmp.Name(), o.path); err != nil {
		return fmt.Errorf("replace overrides: %w", err)
	}
	return nil
}
