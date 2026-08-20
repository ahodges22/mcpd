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

func hasUnitLine(unit, want string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
