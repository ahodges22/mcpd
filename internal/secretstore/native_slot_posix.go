//go:build darwin || linux

package secretstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const nativeHelperLockName = "native-helper.lock"
const nativeLockRetry = 10 * time.Millisecond

var nativeProcessSlot = make(chan struct{}, 1)

type NativeSlot struct {
	dir string
}

type NativeLease struct {
	file *os.File
	once sync.Once
	err  error
}

func NewNativeSlot(stateDir string) (*NativeSlot, error) {
	dir, err := EnsureStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	slot := &NativeSlot{dir: dir}
	lock, err := slot.openLock()
	if err != nil {
		return nil, err
	}
	if err := lock.Close(); err != nil {
		return nil, nativeSlotError(OperationValidate, nativeHelperLockName, ConditionUnexpected, err)
	}
	return slot, nil
}

func (s *NativeSlot) Acquire(ctx context.Context, operation Operation, name string) (*NativeLease, error) {
	if err := ValidateStateDir(s.dir); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nativeSlotError(operation, name, ConditionBusy, err)
	}
	select {
	case nativeProcessSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, nativeSlotError(operation, name, ConditionBusy, ctx.Err())
	}

	lock, err := s.openLock()
	if err != nil {
		<-nativeProcessSlot
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &NativeLease{file: lock}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			<-nativeProcessSlot
			return nil, nativeSlotError(operation, name, ConditionUnexpected, err)
		}
		timer := time.NewTimer(nativeLockRetry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = lock.Close()
			<-nativeProcessSlot
			return nil, nativeSlotError(operation, name, ConditionLockContended, ctx.Err())
		case <-timer.C:
		}
	}
}

func (l *NativeLease) Release() error {
	l.once.Do(func() {
		unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		closeErr := l.file.Close()
		<-nativeProcessSlot
		l.err = errors.Join(unlockErr, closeErr)
	})
	return l.err
}

func (s *NativeSlot) openLock() (*os.File, error) {
	path := filepath.Join(s.dir, nativeHelperLockName)
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
			return nil, nativeSlotError(OperationValidate, nativeHelperLockName, condition, err)
		}
		file := os.NewFile(uintptr(fd), path)
		if created {
			if err := unix.Fchmod(fd, 0o600); err != nil {
				_ = file.Close()
				return nil, nativeSlotError(OperationValidate, nativeHelperLockName, ConditionPermission, err)
			}
		}
		if err := validatePOSIXArtifact("native", nativeHelperLockName, file); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
}

func nativeSlotError(operation Operation, name string, condition Condition, cause error) error {
	return &Error{
		Operation: operation,
		Provider:  "native",
		Name:      name,
		Condition: condition,
		Cause:     cause,
	}
}
