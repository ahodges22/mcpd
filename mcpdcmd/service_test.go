package mcpdcmd

import (
	"os"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(installed, servicepkg.Paths{Binary: filepath.Join(home, "bin", "mcpd"), Config: configPath, State: statePath, Addr: "127.0.0.1:7777"}) {
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

func TestRunServiceInstallPassesDeclarationEnvironmentToSystemd(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	configBody := `{
  "backends": {
    "example": {
      "http_url": "https://example.test/mcp",
      "headers": {"Authorization": "Bearer ${API_TOKEN}"}
    }
  },
  "embeddings": {
    "url": "https://example.test/embeddings",
    "api_key_env": "EMBEDDINGS_TOKEN"
  }
}`
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var unit string
	deps := serviceCommandDeps{
		executable: func() (string, error) { return filepath.Join(home, "bin", "mcpd"), nil },
		install: func(paths servicepkg.Paths) error {
			var err error
			unit, err = servicepkg.RenderSystemdUnit(paths)
			return err
		},
	}

	if err := runService([]string{"install", "--config", configPath, "--state", filepath.Join(home, "state")}, deps); err != nil {
		t.Fatal(err)
	}
	const want = "PassEnvironment=HOME USER LOGNAME SHELL LANG TZ TERM TMPDIR API_TOKEN EMBEDDINGS_TOKEN"
	if !strings.Contains(unit, want) {
		t.Fatalf("generated unit does not pass declaration environment:\n%s", unit)
	}
}

func TestRunServiceInstallRejectsUnsafeEnvironmentName(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	configBody := `{
  "backends": {},
  "embeddings": {
    "url": "https://example.test/embeddings",
    "api_key_env": "TOKEN\nEnvironment=INJECTED=1"
  }
}`
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := serviceCommandDeps{
		executable: func() (string, error) { return filepath.Join(home, "bin", "mcpd"), nil },
		install: func(paths servicepkg.Paths) error {
			_, err := servicepkg.RenderSystemdUnit(paths)
			return err
		},
	}

	err := runService([]string{"install", "--config", configPath, "--state", filepath.Join(home, "state")}, deps)
	if err == nil || !strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("service install error = %v, want unsafe environment variable rejection", err)
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
