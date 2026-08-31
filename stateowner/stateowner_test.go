package stateowner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireInspectConflictReleaseAndStaleFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	status, err := Inspect(state)
	if err != nil || status.Held {
		t.Fatalf("initial inspect = %#v, %v", status, err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect mutated state: %v", err)
	}
	lease, err := Acquire(state, "offline setup")
	if err != nil {
		t.Fatal(err)
	}
	status, err = Inspect(state)
	if err != nil || !status.Held || status.Owner != "offline setup" || status.PID != os.Getpid() {
		t.Fatalf("held inspect = %#v, %v", status, err)
	}
	_, err = Acquire(state, "other")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Owner != "offline setup" {
		t.Fatalf("conflict = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := Acquire(state, "replacement")
	if err != nil {
		t.Fatalf("stale file blocked acquisition: %v", err)
	}
	defer replacement.Close()
}
