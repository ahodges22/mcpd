//go:build darwin || linux

package secretstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFileStoreRoundTrip(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const value = "  quotes='\"' slash=\\ dollar=$ tick=` café-雪-🔐  "
	if err := store.Set(ctx, "TOKEN", value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get(ctx, "TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Present || got.Value != value {
		t.Fatalf("Get = %#v, want byte-exact value", got)
	}

	if err := store.Delete(ctx, "TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = store.Get(ctx, "TOKEN")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if got.Present || got.Value != "" {
		t.Fatalf("Get after Delete = %#v, want clean miss", got)
	}
}

func TestFileStoreConcurrentWritersPreserveBothNames(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	first, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore first: %v", err)
	}
	second, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore second: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, request := range []struct {
		store *FileStore
		name  string
		value string
	}{{first, "FIRST", "one"}, {second, "SECOND", "two"}} {
		request := request
		go func() {
			<-start
			errs <- request.store.Set(ctx, request.name, request.value)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Set: %v", err)
		}
	}

	for name, want := range map[string]string{"FIRST": "one", "SECOND": "two"} {
		got, err := first.Get(ctx, name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if !got.Present || got.Value != want {
			t.Fatalf("Get(%s) = %#v, want %q", name, got, want)
		}
	}
}

func TestFileStoreLockDeadline(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	lock, err := os.OpenFile(filepath.Join(store.dir, fileStoreLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile lock: %v", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("Flock: %v", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = store.Set(ctx, "TOKEN", "value")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Set returned after %s, want bounded return", elapsed)
	}
	assertCondition(t, err, ConditionLockContended)
}

func TestFileStoreReaderSeesCompleteSnapshot(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	reader, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore reader: %v", err)
	}
	writer, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore writer: %v", err)
	}
	oldValue := strings.Repeat("a", 1024)
	newValue := strings.Repeat("b", 1024)
	if err := writer.Set(context.Background(), "TOKEN", oldValue); err != nil {
		t.Fatalf("initial Set: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readerErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				readerErr <- nil
				return
			default:
			}
			got, err := reader.Get(ctx, "TOKEN")
			if err != nil {
				readerErr <- err
				return
			}
			if !got.Present || got.Value != oldValue && got.Value != newValue {
				readerErr <- errors.New("reader observed a partial snapshot")
				return
			}
		}
	}()
	for i := range 20 {
		value := oldValue
		if i%2 == 0 {
			value = newValue
		}
		if err := writer.Set(ctx, "TOKEN", value); err != nil {
			close(stop)
			<-readerErr
			t.Fatalf("Set iteration %d: %v", i, err)
		}
	}
	close(stop)
	if err := <-readerErr; err != nil {
		t.Fatalf("concurrent Get: %v", err)
	}
}

func TestFileStoreRejectsCorruptSnapshot(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	path := filepath.Join(store.dir, fileStoreDataName)
	corrupt := []byte(`{"TOKEN":"truncated"`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("WriteFile corrupt snapshot: %v", err)
	}

	_, err = store.Get(context.Background(), "TOKEN")
	assertCondition(t, err, ConditionCorrupt)
	err = store.Set(context.Background(), "OTHER", "value")
	assertCondition(t, err, ConditionCorrupt)
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile corrupt snapshot: %v", readErr)
	}
	if string(got) != string(corrupt) {
		t.Fatal("Set replaced a corrupt snapshot")
	}
}

func TestFileStoreArtifactsRemainRestrictedAndLockStable(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	lockPath := filepath.Join(store.dir, fileStoreLockName)
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat lock before write: %v", err)
	}
	if got := before.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %o, want 600", got)
	}

	if err := store.Set(context.Background(), "TOKEN", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat lock after write: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("sidecar lock was replaced")
	}
	data, err := os.Stat(filepath.Join(store.dir, fileStoreDataName))
	if err != nil {
		t.Fatalf("Stat data: %v", err)
	}
	if got := data.Mode().Perm(); got != 0o600 {
		t.Fatalf("data mode = %o, want 600", got)
	}
}

func TestFileStoreRejectsUnsafeExistingArtifact(t *testing.T) {
	state, err := EnsureStateDir(filepath.Join(stateSandbox(t), "state"))
	if err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, fileStoreDataName), []byte(`{"TOKEN":"value"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = NewFileStore(state)
	assertCondition(t, err, ConditionPermission)
}

func TestFileStoreValidatesBeforeCreatingSnapshot(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	err = store.Set(context.Background(), "TOKEN", "bad\nvalue")
	assertCondition(t, err, ConditionInvalidValue)
	if _, err := os.Stat(filepath.Join(store.dir, fileStoreDataName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid Set created snapshot: %v", err)
	}
}

func TestFileStoreRevalidatesStateBeforeAccess(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := os.Chmod(store.dir, 0o770); err != nil {
		t.Fatalf("Chmod state: %v", err)
	}

	_, err = store.Get(context.Background(), "TOKEN")
	assertCondition(t, err, ConditionPermission)
	err = store.Set(context.Background(), "TOKEN", "value")
	assertCondition(t, err, ConditionPermission)
	if _, err := os.Stat(filepath.Join(store.dir, fileStoreDataName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe state allowed snapshot creation: %v", err)
	}
}

func TestFileStoreManyConcurrentWriters(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	const writers = 8
	stores := make([]*FileStore, writers)
	for i := range stores {
		store, err := NewFileStore(state)
		if err != nil {
			t.Fatalf("NewFileStore %d: %v", i, err)
		}
		stores[i] = store
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i, store := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Set(ctx, string(rune('A'+i)), "value")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	for i := range writers {
		got, err := stores[0].Get(ctx, string(rune('A'+i)))
		if err != nil || !got.Present {
			t.Fatalf("Get writer %d = %#v, %v", i, got, err)
		}
	}
}
