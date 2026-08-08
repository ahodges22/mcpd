//go:build darwin || linux

package secretstore

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const ControlKeyFile = "secret-control-key"

const controlKeyBytes = 32

type ControlAuthenticator struct {
	key [controlKeyBytes]byte
}

func EnsureControlAuthenticator(stateDir string) (*ControlAuthenticator, error) {
	state, err := EnsureStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(state, ControlKeyFile)
	if _, err := os.Lstat(path); err == nil {
		return LoadControlAuthenticator(state)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, controlAuthError(err)
	}

	var key [controlKeyBytes]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, controlAuthError(err)
	}
	temporary, err := restrictedTemp(state, ".secret-control-key-*")
	if err != nil {
		return nil, controlAuthError(err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, controlAuthError(err)
	}
	encoded := append([]byte(hex.EncodeToString(key[:])), '\n')
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return nil, controlAuthError(err)
	}
	clear(encoded)
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, controlAuthError(err)
	}
	if err := temporary.Close(); err != nil {
		return nil, controlAuthError(err)
	}
	if err := os.Link(temporaryName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			clear(key[:])
			return LoadControlAuthenticator(state)
		}
		return nil, controlAuthError(err)
	}
	if err := syncDirectory(state); err != nil {
		return nil, controlAuthError(err)
	}
	return &ControlAuthenticator{key: key}, nil
}

func LoadControlAuthenticator(stateDir string) (*ControlAuthenticator, error) {
	if err := ValidateStateDir(stateDir); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, ControlKeyFile)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, controlAuthError(err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := validateFileArtifact(ControlKeyFile, file); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, controlKeyBytes*2+2))
	if err != nil {
		return nil, controlAuthError(err)
	}
	if len(raw) != controlKeyBytes*2+1 || raw[len(raw)-1] != '\n' {
		clear(raw)
		return nil, controlAuthError(fmt.Errorf("control key has invalid length"))
	}
	decoded, err := hex.DecodeString(string(raw[:len(raw)-1]))
	clear(raw)
	if err != nil || len(decoded) != controlKeyBytes {
		clear(decoded)
		return nil, controlAuthError(fmt.Errorf("control key is invalid"))
	}
	var key [controlKeyBytes]byte
	copy(key[:], decoded)
	clear(decoded)
	return &ControlAuthenticator{key: key}, nil
}

func NewControlNonce() (string, error) {
	var nonce [controlKeyBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (a *ControlAuthenticator) Proof(nonce string) string {
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = io.WriteString(mac, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *ControlAuthenticator) Verify(nonce, proof string) bool {
	got, err := hex.DecodeString(proof)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(a.Proof(nonce))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func controlAuthError(cause error) error {
	return &Error{
		Operation: OperationValidate,
		Provider:  "control",
		Name:      ControlKeyFile,
		Condition: ConditionPermission,
		Cause:     cause,
	}
}
