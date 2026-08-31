// Package stateowner serializes mutable access to an mcpd state directory.
package stateowner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const lockFile = "runtime.lock"

// Metadata is diagnostic information recorded by the process holding the lock.
// The advisory lock, not these fields, is the ownership authority.
type Metadata struct {
	Owner      string    `json:"owner"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// ConflictError reports the last recorded owner when the state lock is held.
type ConflictError struct {
	Owner string
	PID   int
}

func (e *ConflictError) Error() string {
	if e.Owner == "" && e.PID == 0 {
		return "mcpd state is owned by another process"
	}
	return fmt.Sprintf("mcpd state is owned by %q (pid %d)", e.Owner, e.PID)
}

// Lease is a held ownership lock. Close is idempotent.
type Lease struct {
	mu   sync.Mutex
	file *os.File
	path string
}

var processOwners = struct {
	sync.Mutex
	byPath map[string]Metadata
}{byPath: make(map[string]Metadata)}

// Acquire takes the state lock without waiting.
func Acquire(state, owner string) (*Lease, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, fmt.Errorf("restrict state directory: %w", err)
	}
	realState, err := filepath.EvalSymlinks(state)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	realState, err = filepath.Abs(realState)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute state directory: %w", err)
	}
	path := filepath.Join(realState, lockFile)
	processOwners.Lock()
	if meta, exists := processOwners.byPath[path]; exists {
		processOwners.Unlock()
		return nil, &ConflictError{Owner: meta.Owner, PID: meta.PID}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		processOwners.Unlock()
		return nil, fmt.Errorf("open state ownership lock: %w", err)
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		processOwners.Unlock()
		var meta Metadata
		if raw, readErr := os.ReadFile(path); readErr == nil {
			_ = json.Unmarshal(raw, &meta)
		}
		_ = f.Close()
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EAGAIN) {
			return nil, &ConflictError{Owner: meta.Owner, PID: meta.PID}
		}
		return nil, fmt.Errorf("lock state ownership: %w", err)
	}
	ownerMeta := Metadata{Owner: owner, PID: os.Getpid(), AcquiredAt: time.Now().UTC()}
	processOwners.byPath[path] = ownerMeta
	processOwners.Unlock()
	meta, err := json.Marshal(ownerMeta)
	if err != nil {
		_, _ = release(path, f)
		return nil, fmt.Errorf("encode state ownership: %w", err)
	}
	if err := f.Truncate(0); err == nil {
		_, err = f.WriteAt(append(meta, '\n'), 0)
	}
	if err != nil {
		_, _ = release(path, f)
		return nil, fmt.Errorf("record state ownership: %w", err)
	}
	if err := f.Sync(); err != nil {
		_, _ = release(path, f)
		return nil, fmt.Errorf("sync state ownership: %w", err)
	}
	return &Lease{file: f, path: path}, nil
}

// Inspect returns the recorded metadata and whether another process currently
// owns the state. It never treats stale metadata as ownership.
func Inspect(state string) (Metadata, bool, error) {
	realState, err := filepath.EvalSymlinks(state)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, fmt.Errorf("resolve state directory: %w", err)
	}
	realState, err = filepath.Abs(realState)
	if err != nil {
		return Metadata{}, false, fmt.Errorf("resolve absolute state directory: %w", err)
	}
	path := filepath.Join(realState, lockFile)
	processOwners.Lock()
	defer processOwners.Unlock()
	if meta, ok := processOwners.byPath[path]; ok {
		return meta, true, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, fmt.Errorf("open state ownership lock: %w", err)
	}
	defer f.Close()
	query := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &query); err != nil {
		return Metadata{}, false, fmt.Errorf("inspect state ownership: %w", err)
	}
	locked := query.Type != unix.F_UNLCK
	var meta Metadata
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	return meta, locked, nil
}

// Close releases ownership.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	f := l.file
	released, err := release(l.path, f)
	if released {
		l.file = nil
	}
	return err
}

// release keeps the process-local guard in place until the kernel lock is no
// longer held. A concurrent Acquire may fail spuriously during teardown, but it
// can never receive a lease that an older Close subsequently unlocks.
func release(path string, f *os.File) (bool, error) {
	processOwners.Lock()
	defer processOwners.Unlock()
	lock := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	unlockErr := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock)
	closeErr := f.Close()
	released := unlockErr == nil || closeErr == nil
	if released {
		delete(processOwners.byPath, path)
	}
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock state ownership: %w", unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close state ownership lock: %w", closeErr)
	}
	return released, errors.Join(unlockErr, closeErr)
}
