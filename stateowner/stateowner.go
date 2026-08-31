// Package stateowner provides explicit offline ownership of mcpd state.
package stateowner

import internal "github.com/ahodges22/mcpd/internal/stateowner"

type ConflictError = internal.ConflictError

type Status struct {
	Held  bool   `json:"held"`
	Owner string `json:"owner,omitempty"`
	PID   int    `json:"pid,omitempty"`
}

type Lease struct{ lease *internal.Lease }

// Acquire takes exclusive state ownership without waiting.
func Acquire(state, owner string) (*Lease, error) {
	lease, err := internal.Acquire(state, owner)
	if err != nil {
		return nil, err
	}
	return &Lease{lease: lease}, nil
}

func (l *Lease) Close() error {
	if l == nil || l.lease == nil {
		return nil
	}
	return l.lease.Close()
}

// Inspect reports ownership without creating, acquiring, or modifying a lock.
func Inspect(state string) (Status, error) {
	meta, held, err := internal.Inspect(state)
	if err != nil {
		return Status{}, err
	}
	if !held {
		return Status{}, nil
	}
	return Status{Held: true, Owner: meta.Owner, PID: meta.PID}, nil
}
