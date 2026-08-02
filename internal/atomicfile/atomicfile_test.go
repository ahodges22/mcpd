package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	if err := Write(path, []byte("created"), 0o640); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "created" {
		t.Errorf("content = %q, want %q", got, "created")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %#o, want %#o", got, os.FileMode(0o640))
	}
}

func TestWriteReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %#o, want %#o", got, os.FileMode(0o600))
	}
}
