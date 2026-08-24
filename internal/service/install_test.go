package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRenderSystemdUnitUsesInstalledPathsAndSessionTarget(t *testing.T) {
	unit, err := RenderSystemdUnit(Paths{
		Binary: "/opt/MCP Tools/mcpd",
		Config: "/home/test/${MCPD_CONFIG}/50%/mcpd/config.json",
		State:  "/home/test/.local/state/mcpd",
		Addr:   "127.0.0.1:7777",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ExecStart="/opt/MCP Tools/mcpd" --config "/home/test/$${MCPD_CONFIG}/50%%/mcpd/config.json" --state "/home/test/.local/state/mcpd" --addr "127.0.0.1:7777"`,
		"PartOf=graphical-session.target",
		"WantedBy=graphical-session.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}

func TestRenderLaunchdPlistUsesLoginShellAndEscapedPaths(t *testing.T) {
	plist, err := RenderLaunchdPlist(Paths{
		Binary: "/Users/test/MCP & Tools/mcpd",
		Config: "/Users/test/.config/mcpd/config.json",
		State:  "/Users/test/.local/state/mcpd",
		Addr:   "127.0.0.1:7777",
	}, "/Users/test/Library/Logs/mcpd.log")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		"<string>/bin/zsh</string>",
		"<string>-lc</string>",
		`<string>exec &quot;$@&quot;</string>`,
		"<string>/Users/test/MCP &amp; Tools/mcpd</string>",
		"<string>--config</string>",
		"<string>/Users/test/.config/mcpd/config.json</string>",
		"<string>--state</string>",
		"<string>/Users/test/.local/state/mcpd</string>",
		"<string>--addr</string>",
		"<string>127.0.0.1:7777</string>",
		"<string>/Users/test/Library/Logs/mcpd.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("launchd plist missing %q:\n%s", want, plist)
		}
	}
}

func TestRenderServiceDefinitionRejectsControlCharactersInPaths(t *testing.T) {
	paths := Paths{Binary: "/opt/mcpd\nEnvironment=INJECTED=1", Config: "/tmp/config", State: "/tmp/state", Addr: "127.0.0.1:7420"}
	if _, err := RenderSystemdUnit(paths); err == nil {
		t.Fatal("systemd renderer accepted a newline in the binary path")
	}
	if _, err := RenderLaunchdPlist(paths, "/tmp/mcpd.log"); err == nil {
		t.Fatal("launchd renderer accepted a newline in the binary path")
	}
}

func TestInstallSystemdWritesAndStartsUserService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	setPlatform(t, "linux")
	var calls [][]string
	setRunner(t, func(name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		return "", nil
	})
	paths := Paths{Binary: "/opt/mcpd", Config: "/tmp/config.json", State: "/tmp/state", Addr: "127.0.0.1:7777"}

	if err := Install(paths); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", "mcpd.service")
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `ExecStart="/opt/mcpd" --config "/tmp/config.json" --state "/tmp/state" --addr "127.0.0.1:7777"`) {
		t.Fatalf("installed unit has wrong command:\n%s", body)
	}
	want := [][]string{
		{"systemctl", "--user", "is-active", "--quiet", "graphical-session.target"},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", "mcpd.service"},
		{"systemctl", "--user", "restart", "mcpd.service"},
	}
	if !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("service calls = %v, want %v", calls, want)
	}
}

func TestInstallSystemdRefusesToStartOutsideGraphicalSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	setPlatform(t, "linux")
	setRunner(t, func(_ string, args ...string) (string, error) {
		if slices.Equal(args, []string{"--user", "is-active", "--quiet", "graphical-session.target"}) {
			return "inactive\n", errors.New("exit status 3")
		}
		return "", nil
	})

	err := Install(Paths{Binary: "/opt/mcpd", Config: "/tmp/config.json", State: "/tmp/state", Addr: "127.0.0.1:7420"})
	if err == nil || !strings.Contains(err.Error(), "graphical session") {
		t.Fatalf("Install error = %v", err)
	}
	path, pathErr := systemdPath()
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("service file exists after refused install: %v", statErr)
	}
}

func TestInstallLaunchdReloadsAgentAndCreatesLogDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setPlatform(t, "darwin")
	var calls [][]string
	setRunner(t, func(name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && (args[0] == "bootout" || args[0] == "print") {
			return "", errors.New("Boot-out failed: 3: No such process")
		}
		return "", nil
	})

	if err := Install(Paths{Binary: "/opt/mcpd", Config: "/tmp/config.json", State: "/tmp/state", Addr: "127.0.0.1:7420"}); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "dev.mcpd.daemon.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(home, "Library", "Logs")
	if info, err := os.Stat(logDir); err != nil || !info.IsDir() {
		t.Fatalf("log directory: info=%v err=%v", info, err)
	}
	domain := "gui/" + userID()
	want := [][]string{
		{"launchctl", "bootout", domain + "/dev.mcpd.daemon"},
		{"launchctl", "print", domain + "/dev.mcpd.daemon"},
		{"launchctl", "enable", domain + "/dev.mcpd.daemon"},
		{"launchctl", "bootstrap", domain, plist},
	}
	if !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("service calls = %v, want %v", calls, want)
	}
}

func TestInstallLaunchdWaitsForPreviousAgentToUnload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setPlatform(t, "darwin")
	printCalls := 0
	setRunner(t, func(_ string, args ...string) (string, error) {
		switch {
		case len(args) > 0 && args[0] == "bootout":
			return "", nil
		case len(args) > 0 && args[0] == "print":
			printCalls++
			if printCalls == 1 {
				return "service is still unloading", nil
			}
			return "", errors.New("Could not find service")
		case len(args) > 0 && args[0] == "bootstrap" && printCalls < 2:
			return "", errors.New("bootstrap raced with launchd removal")
		default:
			return "", nil
		}
	})

	if err := Install(Paths{Binary: "/opt/mcpd", Config: "/tmp/config.json", State: "/tmp/state", Addr: "127.0.0.1:7420"}); err != nil {
		t.Fatal(err)
	}
}

func setPlatform(t *testing.T, value string) {
	t.Helper()
	original := platform
	platform = value
	t.Cleanup(func() { platform = original })
}

func setRunner(t *testing.T, value func(string, ...string) (string, error)) {
	t.Helper()
	original := runner
	runner = value
	t.Cleanup(func() { runner = original })
}
