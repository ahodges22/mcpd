//go:build darwin || linux

package secretstore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestValidateStateDirPOSIX(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "nested", "state")
	resolved, err := EnsureStateDir(state)
	if err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	if err := ValidateStateDir(resolved); err != nil {
		t.Fatalf("ValidateStateDir: %v", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("state mode = %o, want 700", got)
	}

	if err := os.Chmod(resolved, 0o750); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	assertCondition(t, ValidateStateDir(resolved), ConditionPermission)

	if err := os.Chmod(resolved, 0o700); err != nil {
		t.Fatalf("Chmod restore: %v", err)
	}
	priorUID := effectiveUID
	effectiveUID = func() int { return priorUID() + 1 }
	t.Cleanup(func() { effectiveUID = priorUID })
	assertCondition(t, ValidateStateDir(resolved), ConditionPermission)
}

func TestValidateStateDirPOSIXChecksParentsOfRelativePath(t *testing.T) {
	unsafeParent := filepath.Join(stateSandbox(t), "unsafe")
	working := filepath.Join(unsafeParent, "working")
	state := filepath.Join(working, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatalf("Chmod unsafe parent: %v", err)
	}
	if err := os.Chmod(working, 0o700); err != nil {
		t.Fatalf("Chmod working: %v", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatalf("Chmod state: %v", err)
	}
	t.Chdir(working)

	assertCondition(t, ValidateStateDir("state"), ConditionPermission)
}

func TestUnsafeParentFailsBeforeArtifactRead(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, os.ModeSticky | 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			unsafeParent := filepath.Join(stateSandbox(t), "unsafe")
			if err := os.Mkdir(unsafeParent, 0o700); err != nil {
				t.Fatalf("Mkdir parent: %v", err)
			}
			if err := os.Chmod(unsafeParent, mode); err != nil {
				t.Fatalf("Chmod parent: %v", err)
			}
			state := filepath.Join(unsafeParent, "state")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatalf("Mkdir state: %v", err)
			}
			artifact := filepath.Join(state, "native-helper.json")
			if err := os.WriteFile(artifact, []byte("not valid json"), 0); err != nil {
				t.Fatalf("WriteFile artifact: %v", err)
			}

			assertCondition(t, ValidateStateDir(state), ConditionPermission)
		})
	}
}

func TestEnsureStateDirRejectsSymlinkPlantedDuringCreation(t *testing.T) {
	root := stateSandbox(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	state := filepath.Join(root, "state")
	prior := beforeStateMkdir
	planted := false
	beforeStateMkdir = func(path string) {
		if filepath.Base(path) == "state" {
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			planted = true
		}
	}
	t.Cleanup(func() { beforeStateMkdir = prior })

	assertCondition(t, func() error {
		_, err := EnsureStateDir(state)
		return err
	}(), ConditionPermission)
	if !planted {
		t.Fatal("test did not reach the state-directory creation window")
	}
}

func TestValidateStateDirRejectsExistingSymlink(t *testing.T) {
	root := stateSandbox(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	state := filepath.Join(root, "state")
	if err := os.Symlink(target, state); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	assertCondition(t, ValidateStateDir(state), ConditionPermission)
}

func TestValidateStateDirChecksLiteralParentChain(t *testing.T) {
	root := stateSandbox(t)
	targetParent := filepath.Join(root, "target")
	targetState := filepath.Join(targetParent, "state")
	if err := os.MkdirAll(targetState, 0o700); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o700); err != nil {
		t.Fatalf("Mkdir unsafe parent: %v", err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatalf("Chmod unsafe parent: %v", err)
	}
	link := filepath.Join(unsafeParent, "link")
	if err := os.Symlink(targetParent, link); err != nil {
		t.Fatalf("Symlink parent: %v", err)
	}

	assertCondition(t, ValidateStateDir(filepath.Join(link, "state")), ConditionPermission)
}

func TestEnsureStateDirChecksLiteralParentChain(t *testing.T) {
	root := stateSandbox(t)
	targetParent := filepath.Join(root, "target")
	if err := os.Mkdir(targetParent, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o700); err != nil {
		t.Fatalf("Mkdir unsafe parent: %v", err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatalf("Chmod unsafe parent: %v", err)
	}
	link := filepath.Join(unsafeParent, "link")
	if err := os.Symlink(targetParent, link); err != nil {
		t.Fatalf("Symlink parent: %v", err)
	}

	assertCondition(t, func() error {
		_, err := EnsureStateDir(filepath.Join(link, "state"))
		return err
	}(), ConditionPermission)
	if _, err := os.Stat(filepath.Join(targetParent, "state")); !os.IsNotExist(err) {
		t.Fatalf("redirected state directory was created: %v", err)
	}
}

func TestRestrictedTempPrecedesSecretWrite(t *testing.T) {
	state, err := EnsureStateDir(filepath.Join(stateSandbox(t), "state"))
	if err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	tmp, err := restrictedTemp(state, ".secret-*")
	if err != nil {
		t.Fatalf("restrictedTemp: %v", err)
	}
	t.Cleanup(func() {
		tmp.Close()
		os.Remove(tmp.Name())
	})

	info, err := tmp.Stat()
	if err != nil {
		t.Fatalf("Stat before write: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("temporary mode before secret write = %o, want 600", got)
	}
	if info.Size() != 0 {
		t.Fatalf("temporary size before secret write = %d, want 0", info.Size())
	}
}

func TestEnsureStateDirIgnoresUmask(t *testing.T) {
	root := stateSandbox(t)
	prior := syscall.Umask(0o777)
	t.Cleanup(func() { syscall.Umask(prior) })

	state, err := EnsureStateDir(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("EnsureStateDir: %v", err)
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("state mode = %o, want 700", got)
	}
}

func assertCondition(t *testing.T, err error, want Condition) {
	t.Helper()
	if got, ok := ConditionOf(err); !ok || got != want {
		t.Fatalf("condition = %q, %v; want %q; error: %v", got, ok, want, err)
	}
}

func stateSandbox(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory for safe-parent state tests: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".mcpd-statetest-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Chmod sandbox: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
