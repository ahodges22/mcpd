package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/config"
)

func TestRunSetupInstallsServiceChecksHealthAndRewiresDetectedClients(t *testing.T) {
	home := t.TempDir()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".config", "mcpd", "config.json")
	statePath := filepath.Join(home, ".local", "state", "mcpd")
	var order []string
	var output strings.Builder
	deps := setupCommandDeps{
		homeDir: func() (string, error) { return home, nil },
		installService: func(configPath, statePath, addr string) error {
			order = append(order, "service")
			if addr != "127.0.0.1:7777" {
				t.Fatalf("service addr = %q", addr)
			}
			if _, _, err := config.NewWriter(configPath); err != nil {
				return err
			}
			return os.MkdirAll(statePath, 0o700)
		},
		doctor: func(configPath, addr string) error {
			order = append(order, "doctor")
			return nil
		},
		stdin:  strings.NewReader("y\n"),
		stdout: &output,
	}

	if err := runSetup([]string{"--config", configPath, "--state", statePath, "--addr", "127.0.0.1:7777"}, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "service,doctor" {
		t.Fatalf("operation order = %v", order)
	}
	body, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "[mcp_servers.mcpd]") {
		t.Fatalf("Codex was not rewired:\n%s", body)
	}
	for _, absent := range []string{".claude.json", ".cursor/mcp.json", ".config/opencode/opencode.json"} {
		if _, err := os.Stat(filepath.Join(home, absent)); !os.IsNotExist(err) {
			t.Fatalf("setup created absent client config %s: %v", absent, err)
		}
	}
	if got := output.String(); !strings.Contains(got, "Detected client: codex") || !strings.Contains(got, "Apply these client changes? [y/N]") {
		t.Fatalf("setup output = %q", got)
	}
	installed := string(body)
	if err := runSetup([]string{"--yes", "--config", configPath, "--state", statePath, "--addr", "127.0.0.1:7777"}, deps); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if body, err := os.ReadFile(codexPath); err != nil || string(body) != installed {
		t.Fatalf("second setup changed Codex config to %q, err=%v", body, err)
	}
}

func TestRunSetupDeclineLeavesClientConfigUntouched(t *testing.T) {
	home := t.TempDir()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "model = \"gpt-5\"\n"
	if err := os.WriteFile(codexPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := setupCommandDeps{
		homeDir:        func() (string, error) { return home, nil },
		installService: func(string, string, string) error { return nil },
		doctor:         func(string, string) error { return nil },
		stdin:          strings.NewReader("n\n"),
		stdout:         &strings.Builder{},
	}
	configPath := filepath.Join(home, ".config", "mcpd", "config.json")
	statePath := filepath.Join(home, ".local", "state", "mcpd")

	if err := runSetup([]string{"--config", configPath, "--state", statePath}, deps); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(codexPath); err != nil || string(body) != original {
		t.Fatalf("declined setup changed config to %q, err=%v", body, err)
	}
}
