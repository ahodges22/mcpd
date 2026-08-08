//go:build darwin || linux

package secretstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	fileStoreDataName = "secrets.json"
	fileStoreLockName = "secrets.lock"
	fileLockRetry     = 10 * time.Millisecond
)

type FileStore struct {
	dir                string
	observeMu          sync.Mutex
	observed           fileObservation
	disableWatchEvents bool
}

func NewFileStore(stateDir string) (*FileStore, error) {
	dir, err := EnsureStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	store := &FileStore{dir: dir}
	values, data, metadata, err := store.loadSnapshot(OperationGet, "")
	if err != nil {
		return nil, err
	}
	store.observed = observeFileSnapshot(data, values, metadata)
	clear(values)
	lock, err := store.openLock()
	if err != nil {
		return nil, err
	}
	if err := lock.Close(); err != nil {
		return nil, fileStoreError(OperationValidate, "", ConditionUnexpected, err)
	}
	return store, nil
}

func (s *FileStore) Get(ctx context.Context, name string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fileStoreError(OperationGet, name, ConditionTimedOut, err)
	}
	if err := ValidateStateDir(s.dir); err != nil {
		return Result{}, err
	}
	values, err := s.readSnapshot(OperationGet, name)
	if err != nil {
		return Result{}, err
	}
	value, ok := values[name]
	return Result{Value: value, Present: ok}, nil
}

func (s *FileStore) Set(ctx context.Context, name, value string) error {
	if err := ValidateValue(value); err != nil {
		return err
	}
	return s.mutate(ctx, OperationSet, name, func(values map[string]string) bool {
		values[name] = value
		return true
	})
}

func (s *FileStore) Delete(ctx context.Context, name string) error {
	return s.mutate(ctx, OperationDelete, name, func(values map[string]string) bool {
		if _, ok := values[name]; !ok {
			return false
		}
		delete(values, name)
		return true
	})
}

func (s *FileStore) mutate(ctx context.Context, operation Operation, name string, change func(map[string]string) bool) error {
	if err := ValidateStateDir(s.dir); err != nil {
		return err
	}
	lock, err := s.acquireLock(ctx, operation, name)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}()

	values, err := s.readSnapshot(operation, name)
	if err != nil {
		return err
	}
	if !change(values) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fileStoreError(operation, name, ConditionTimedOut, err)
	}
	return s.writeSnapshot(ctx, operation, name, values)
}

func (s *FileStore) acquireLock(ctx context.Context, operation Operation, name string) (*os.File, error) {
	lock, err := s.openLock()
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, fileStoreError(operation, name, ConditionUnexpected, err)
		}

		timer := time.NewTimer(fileLockRetry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = lock.Close()
			return nil, fileStoreError(operation, name, ConditionLockContended, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *FileStore) openLock() (*os.File, error) {
	path := filepath.Join(s.dir, fileStoreLockName)
	for {
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		created := false
		if errors.Is(err, unix.ENOENT) {
			fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CREAT|unix.O_EXCL, 0o600)
			created = err == nil
			if errors.Is(err, unix.EEXIST) {
				continue
			}
		}
		if err != nil {
			condition := ConditionUnexpected
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EACCES) {
				condition = ConditionPermission
			}
			return nil, fileStoreError(OperationValidate, fileStoreLockName, condition, err)
		}
		file := os.NewFile(uintptr(fd), path)
		if created {
			if err := unix.Fchmod(fd, 0o600); err != nil {
				_ = file.Close()
				return nil, fileStoreError(OperationValidate, fileStoreLockName, ConditionPermission, err)
			}
		}
		if err := validateFileArtifact(fileStoreLockName, file); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
}

func (s *FileStore) readSnapshot(operation Operation, name string) (map[string]string, error) {
	values, _, _, err := s.loadSnapshot(operation, name)
	return values, err
}

func (s *FileStore) loadSnapshot(operation Operation, name string) (map[string]string, []byte, fileMetadata, error) {
	path := filepath.Join(s.dir, fileStoreDataName)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return make(map[string]string), nil, fileMetadata{}, nil
	}
	if err != nil {
		condition := ConditionUnexpected
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EACCES) {
			condition = ConditionPermission
		}
		return nil, nil, fileMetadata{}, fileStoreError(operation, name, condition, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := validateFileArtifact(fileStoreDataName, file); err != nil {
		return nil, nil, fileMetadata{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fileMetadata{}, fileStoreError(operation, name, ConditionUnexpected, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, fileMetadata{}, fileStoreError(operation, name, ConditionUnexpected, err)
	}
	values, err := decodeSnapshot(data)
	if err != nil {
		return nil, nil, fileMetadata{}, fileStoreError(operation, name, ConditionCorrupt, err)
	}
	return values, data, metadataFromFileInfo(info), nil
}

func decodeSnapshot(data []byte) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var values map[string]string
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	if values == nil {
		return nil, fmt.Errorf("snapshot is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("snapshot contains multiple values")
		}
		return nil, err
	}
	for name, value := range values {
		if err := ValidateValue(value); err != nil {
			return nil, fmt.Errorf("stored name %q has an invalid value: %w", name, err)
		}
	}
	return values, nil
}

func (s *FileStore) writeSnapshot(ctx context.Context, operation Operation, name string, values map[string]string) error {
	data, err := json.Marshal(values)
	if err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	data = append(data, '\n')
	tmp, err := restrictedTemp(s.dir, ".secrets-*")
	if err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fileStoreError(operation, name, ConditionPermission, err)
	}
	if err := ctx.Err(); err != nil {
		return fileStoreError(operation, name, ConditionTimedOut, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	if err := tmp.Sync(); err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	if err := tmp.Close(); err != nil {
		closed = true
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return fileStoreError(operation, name, ConditionTimedOut, err)
	}
	s.observeMu.Lock()
	defer s.observeMu.Unlock()
	if err := os.Rename(tmpName, filepath.Join(s.dir, fileStoreDataName)); err != nil {
		return fileStoreError(operation, name, ConditionUnexpected, err)
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return fileStoreError(operation, name, ConditionIndeterminate, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fileStoreError(operation, name, ConditionIndeterminate, err)
	}
	if err := dir.Close(); err != nil {
		return fileStoreError(operation, name, ConditionIndeterminate, err)
	}
	s.observed = observeFileSnapshot(data, values, metadataFromFileInfo(info))
	return nil
}

func validateFileArtifact(name string, file *os.File) error {
	return validatePOSIXArtifact("file", name, file)
}

func validatePOSIXArtifact(provider, name string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return artifactPermissionError(provider, name, err)
	}
	if !info.Mode().IsRegular() {
		return artifactPermissionError(provider, name, fmt.Errorf("artifact is not a regular file"))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return artifactPermissionError(provider, name, fmt.Errorf("POSIX ownership is unavailable"))
	}
	if int(stat.Uid) != effectiveUID() {
		return artifactPermissionError(provider, name, fmt.Errorf("owned by uid %d, current uid is %d", stat.Uid, effectiveUID()))
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 || mode&0o600 != 0o600 {
		return artifactPermissionError(provider, name, fmt.Errorf("mode %04o is not owner-only read-write", mode))
	}
	return nil
}

func artifactPermissionError(provider, name string, cause error) error {
	return &Error{Operation: OperationValidate, Provider: provider, Name: name, Condition: ConditionPermission, Cause: cause}
}

func fileStoreError(operation Operation, name string, condition Condition, cause error) error {
	return &Error{
		Operation: operation,
		Provider:  "file",
		Name:      name,
		Condition: condition,
		Cause:     cause,
	}
}
