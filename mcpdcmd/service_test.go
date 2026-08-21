package mcpdcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	servicepkg "github.com/ahodges22/mcpd/internal/service"
)

func TestRunServiceInstallBootstrapsPathsAndInstalls(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "mcpd", "config.json")
	statePath := filepath.Join(home, ".local", "state", "mcpd")
	var installed servicepkg.Paths
	var output strings.Builder
	deps := serviceCommandDeps{
		executable: func() (string, error) { return filepath.Join(home, "bin", "mcpd"), nil },
		install: func(paths servicepkg.Paths) error {
			installed = paths
			return nil
		},
		stdout: &output,
	}

	if err := runService([]string{"install", "--config", configPath, "--state", statePath, "--addr", "127.0.0.1:7777"}, deps); err != nil {
		t.Fatal(err)
	}
	if installed != (servicepkg.Paths{Binary: filepath.Join(home, "bin", "mcpd"), Config: configPath, State: statePath, Addr: "127.0.0.1:7777"}) {
		t.Fatalf("installed paths = %+v", installed)
	}
	if body, err := os.ReadFile(configPath); err != nil || string(body) != "{\"backends\":{}}\n" {
		t.Fatalf("initial config = %q, err=%v", body, err)
	}
	if info, err := os.Stat(statePath); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory: info=%v err=%v", info, err)
	}
	if !strings.Contains(output.String(), "installed and started") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunServiceStatusReportsState(t *testing.T) {
	var output strings.Builder
	deps := serviceCommandDeps{
		inspect: func() (servicepkg.State, error) {
			return servicepkg.State{Installed: true, Enabled: true, Running: true}, nil
		},
		stdout: &output,
	}

	if err := runService([]string{"status"}, deps); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "mcpd service: installed, enabled, running\n" {
		t.Fatalf("output = %q", got)
	}
}
