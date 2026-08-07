//go:build darwin || linux

package secretstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var effectiveUID = os.Geteuid
var beforeStateMkdir func(string)

func EnsureStateDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", statePermissionError(path, err)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err == nil {
		if err := ValidateStateDir(abs); err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", statePermissionError(abs, err)
		}
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", statePermissionError(abs, err)
	}

	ancestor := abs
	var missing []string
	for {
		missing = append(missing, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
		if _, err := os.Stat(ancestor); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", statePermissionError(ancestor, err)
		}
	}
	if err := validateSafeParents(ancestor); err != nil {
		return "", err
	}
	resolvedAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", statePermissionError(ancestor, err)
	}
	ancestor = resolvedAncestor
	if err := validateSafeParents(ancestor); err != nil {
		return "", err
	}

	current := ancestor
	for i := len(missing) - 1; i >= 0; i-- {
		current = filepath.Join(current, missing[i])
		if beforeStateMkdir != nil {
			beforeStateMkdir(current)
		}
		if err := os.Mkdir(current, 0o700); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", statePermissionError(current, err)
			}
			info, lstatErr := os.Lstat(current)
			if lstatErr != nil {
				return "", statePermissionError(current, lstatErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", statePermissionError(current, fmt.Errorf("path component is not a real directory"))
			}
		} else if err := os.Chmod(current, 0o700); err != nil {
			return "", statePermissionError(current, err)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return "", statePermissionError(current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", statePermissionError(current, fmt.Errorf("path component is not a real directory"))
		}
		if err := validateOwnerAndMode(current, info, true); err != nil {
			return "", err
		}
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", statePermissionError(current, err)
	}
	if err := ValidateStateDir(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func ValidateStateDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return statePermissionError(path, err)
	}
	if err := validateSafeParents(filepath.Dir(abs)); err != nil {
		return err
	}
	literal, err := os.Lstat(abs)
	if err != nil {
		return statePermissionError(abs, err)
	}
	if literal.Mode()&os.ModeSymlink != 0 {
		return statePermissionError(abs, fmt.Errorf("state path is a symbolic link"))
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return statePermissionError(path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return statePermissionError(resolved, err)
	}
	if !info.IsDir() {
		return statePermissionError(resolved, fmt.Errorf("state path is not a directory"))
	}
	if err := validateOwnerAndMode(resolved, info, true); err != nil {
		return err
	}
	return validateSafeParents(filepath.Dir(resolved))
}

func validateSafeParents(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return statePermissionError(current, err)
		}
		if !info.IsDir() {
			return statePermissionError(current, fmt.Errorf("parent is not a directory"))
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return statePermissionError(current, fmt.Errorf("POSIX ownership is unavailable"))
		}
		if int(stat.Uid) != 0 && int(stat.Uid) != effectiveUID() {
			return statePermissionError(current, fmt.Errorf("parent is owned by untrusted uid %d", stat.Uid))
		}
		if info.Mode().Perm()&0o022 != 0 {
			return statePermissionError(current, fmt.Errorf("parent is writable by group or other"))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func validateOwnerAndMode(path string, info os.FileInfo, requireOwnerAccess bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return statePermissionError(path, fmt.Errorf("POSIX ownership is unavailable"))
	}
	if int(stat.Uid) != effectiveUID() {
		return statePermissionError(path, fmt.Errorf("owned by uid %d, current uid is %d", stat.Uid, effectiveUID()))
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return statePermissionError(path, fmt.Errorf("mode %04o grants group or other access", mode))
	}
	if requireOwnerAccess && mode&0o700 != 0o700 {
		return statePermissionError(path, fmt.Errorf("mode %04o does not grant owner rwx", mode))
	}
	return nil
}

func restrictedTemp(dir, pattern string) (*os.File, error) {
	if filepath.Base(pattern) != pattern || strings.Count(pattern, "*") != 1 {
		return nil, fmt.Errorf("temporary pattern must be one base name containing one *")
	}
	for range 100 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, fmt.Errorf("generate temporary name: %w", err)
		}
		name := strings.Replace(pattern, "*", hex.EncodeToString(nonce[:]), 1)
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create restricted temporary file: %w", err)
		}
	}
	return nil, fmt.Errorf("create restricted temporary file: name collisions")
}

func statePermissionError(path string, cause error) error {
	return &Error{
		Operation: OperationValidate,
		Provider:  "state",
		Name:      path,
		Condition: ConditionPermission,
		Cause:     cause,
	}
}
