package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectSystemdReportsInstalledAndRunning(t *testing.T) {
	setPlatform(t, "linux")
	setRunner(t, func(name string, args ...string) (string, error) {
		return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\n", nil
	})

	state, err := Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Installed || !state.Enabled || !state.Running {
		t.Fatalf("state = %+v, want installed, enabled, and running", state)
	}
}

func TestParseSystemdActivatingIsNotRunning(t *testing.T) {
	state, err := parseSystemdState("LoadState=loaded\nUnitFileState=enabled\nActiveState=activating\n")
	if err != nil {
		t.Fatal(err)
	}
	if state.Running {
		t.Fatal("activating service reported as running")
	}
}

func TestInspectLaunchdDistinguishesLoadedFromRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setPlatform(t, "darwin")
	path, err := launchdPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	setRunner(t, func(string, ...string) (string, error) {
		return "state = exited\nlast exit code = 1\n", nil
	})

	state, err := Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Installed || !state.Enabled || state.Running {
		t.Fatalf("state = %+v, want loaded but not running", state)
	}
}

func TestUninstallSystemdRemovesUnitWhenStopFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	setPlatform(t, "linux")
	path, err := systemdPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	setRunner(t, func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[1] == "disable" {
			return "", errors.New("stop failed")
		}
		return "", nil
	})

	err = Uninstall()
	if err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("Uninstall error = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("service file remains after uninstall: %v", statErr)
	}
}

func TestUninstallSystemdIsIdempotentWhenUnitIsAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	setPlatform(t, "linux")
	setRunner(t, func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[1] == "disable" {
			return "Unit mcpd.service does not exist", errors.New("exit status 5")
		}
		return "", nil
	})

	if err := Uninstall(); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
}
