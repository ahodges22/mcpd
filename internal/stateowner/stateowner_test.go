package stateowner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestSecondOwnerIsRefusedAndCloseReleasesOwnership(t *testing.T) {
	state := t.TempDir()
	first, err := Acquire(state, "odo")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(state, "standalone")
	if second != nil {
		t.Fatal("second owner acquired the lock")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Owner != "odo" || conflict.PID != os.Getpid() {
		t.Fatalf("got %#v, want odo ownership conflict", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestStateOwnerSubprocessSeesConflict")
	cmd.Env = append(os.Environ(), "MCPD_STATEOWNER_HELPER=1", "MCPD_STATEOWNER_PATH="+state)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess acquired lock after same-process conflict: %v\n%s", err, output)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := Acquire(state, "standalone")
	if err != nil {
		t.Fatalf("stale lock file blocked acquisition: %v", err)
	}
	defer replacement.Close()
	if _, err := os.Stat(filepath.Join(state, lockFile)); err != nil {
		t.Fatal(err)
	}
}

func TestStateOwnerSubprocessSeesConflict(t *testing.T) {
	if os.Getenv("MCPD_STATEOWNER_HELPER") != "1" {
		return
	}
	lease, err := Acquire(os.Getenv("MCPD_STATEOWNER_PATH"), "child")
	if lease != nil {
		lease.Close()
		t.Fatal("child acquired held state")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("child acquisition = %v", err)
	}
}

func TestLeaseCloseIsConcurrentAndIdempotent(t *testing.T) {
	state := t.TempDir()
	lease, err := Acquire(state, "odo")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- lease.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	replacement, err := Acquire(state, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
}

func TestInspectDoesNotCreateOrAcquireALock(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	_, held, err := Inspect(state)
	if err != nil || held {
		t.Fatalf("Inspect = held %v, error %v", held, err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect mutated state: %v", err)
	}
	lease, err := Acquire(state, "odo")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	meta, held, err := Inspect(state)
	if err != nil || !held || meta.Owner != "odo" {
		t.Fatalf("Inspect = %#v, %v, %v", meta, held, err)
	}
}

func TestSymlinkedStatePathCannotBypassProcessOwnership(t *testing.T) {
	realState := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(realState, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(realState, alias); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(realState, "odo")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, err = Acquire(alias, "standalone")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Owner != "odo" {
		t.Fatalf("symlink acquisition = %v", err)
	}
	meta, held, err := Inspect(alias)
	if err != nil || !held || meta.Owner != "odo" {
		t.Fatalf("symlink inspect = %#v, %v, %v", meta, held, err)
	}
}
