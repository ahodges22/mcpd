package main

import (
	"os"
	"strings"
	"testing"
)

func TestLinuxTrayService(t *testing.T) {
	raw, err := os.ReadFile("../../dist/mcpd-tray.service")
	if err != nil {
		t.Fatalf("read Linux tray service: %v", err)
	}
	unit := string(raw)
	wantLines := []string{
		"PartOf=graphical-session.target",
		"StartLimitIntervalSec=60s",
		"StartLimitBurst=3",
		"Type=exec",
		"Restart=on-failure",
		"RestartSec=5s",
		"WantedBy=graphical-session.target",
	}
	for _, line := range wantLines {
		if !hasUnitLine(unit, line) {
			t.Errorf("Linux tray service missing %q", line)
		}
	}

	const execStart = "ExecStart=/usr/bin/env -i HOME=%h PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin XDG_RUNTIME_DIR=%t DBUS_SESSION_BUS_ADDRESS=unix:path=%t/bus DISPLAY=${DISPLAY} WAYLAND_DISPLAY=${WAYLAND_DISPLAY} XAUTHORITY=${XAUTHORITY} XDG_CURRENT_DESKTOP=${XDG_CURRENT_DESKTOP} XDG_SESSION_TYPE=${XDG_SESSION_TYPE} LANG=${LANG} %h/.local/bin/mcpd tray"
	if !hasUnitLine(unit, execStart) {
		t.Error("Linux tray service does not use the exact desktop-only environment boundary")
	}
	for _, forbidden := range []string{"PassEnvironment=", "EnvironmentFile=", "/bin/sh", "/bin/bash", "/bin/zsh"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("Linux tray service contains forbidden %q", forbidden)
		}
	}
}

func TestMacOSTrayLaunchAgent(t *testing.T) {
	raw, err := os.ReadFile("../../dist/dev.mcpd.tray.plist")
	if err != nil {
		t.Fatalf("read macOS tray LaunchAgent: %v", err)
	}
	plist := string(raw)
	wantFragments := []string{
		"<key>Label</key>\n  <string>dev.mcpd.tray</string>",
		"<key>LimitLoadToSessionType</key>\n  <string>Aqua</string>",
		"<key>SuccessfulExit</key>\n    <false/>",
		"<key>ThrottleInterval</key>\n  <integer>10</integer>",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(plist, fragment) {
			t.Errorf("macOS tray LaunchAgent missing %q", fragment)
		}
	}

	const programArguments = `<key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>exec /usr/bin/env -i HOME="$HOME" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-en_US.UTF-8}" "$HOME/.local/bin/mcpd" tray</string>
  </array>`
	if !strings.Contains(plist, programArguments) {
		t.Error("macOS tray LaunchAgent does not use the exact minimal environment boundary")
	}
	for _, forbidden := range []string{"<key>RunAtLoad</key>", "<key>EnvironmentVariables</key>", "-lc", "/bin/zsh", ".zprofile", ".profile", "dev.mcpd.daemon"} {
		if strings.Contains(plist, forbidden) {
			t.Errorf("macOS tray LaunchAgent contains forbidden %q", forbidden)
		}
	}
}

func hasUnitLine(unit, want string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
