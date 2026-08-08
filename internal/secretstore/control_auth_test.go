//go:build darwin || linux

package secretstore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestControlAuthenticatorPersistsOwnerOnlyChallengeKey(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	oldUmask := syscall.Umask(0o277)
	defer syscall.Umask(oldUmask)
	created, err := EnsureControlAuthenticator(state)
	if err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	loaded, err := LoadControlAuthenticator(state)
	if err != nil {
		t.Fatalf("LoadControlAuthenticator: %v", err)
	}
	nonce, err := NewControlNonce()
	if err != nil {
		t.Fatalf("NewControlNonce: %v", err)
	}
	proof := created.Proof(nonce)
	if !loaded.Verify(nonce, proof) {
		t.Fatal("loaded authenticator rejected the persisted proof")
	}
	info, err := os.Stat(filepath.Join(state, ControlKeyFile))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("control key mode = %04o", info.Mode().Perm())
	}
}

func TestControlAuthenticatorRejectsWrongProof(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	auth, err := EnsureControlAuthenticator(state)
	if err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	if auth.Verify("nonce", "00") {
		t.Fatal("wrong proof was accepted")
	}
}
