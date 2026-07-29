package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// wantBaseEnvKeys is the curated set ChildEnv is expected to forward to every stdio
// child, regardless of what a backend declares. It is spelled out here, rather than
// imported, so the test fixes the contract instead of following the implementation.
var wantBaseEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM",
	"TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
}

func isBaseKey(k string) bool {
	return slices.Contains(wantBaseEnvKeys, k)
}

// Scenario: A declared credential is granted (backend-sessions spec, "Least-privilege
// stdio child environment").
func TestChildEnv_GrantsDeclaredPassthrough(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/alex",
		"AWS_PROFILE=mgmt", "KUBECONFIG=/home/alex/.kube/config",
		"GH_PAT=secret-pat",
	}
	art := Backend{
		Name:           "art",
		Command:        "/usr/local/bin/art-mcp-server",
		EnvPassthrough: []string{"AWS_*", "KUBECONFIG"},
	}

	got := art.ChildEnv(parent)

	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/home/alex",
		"AWS_PROFILE=mgmt", "KUBECONFIG=/home/alex/.kube/config",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in child env %v", want, got)
		}
	}
	if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "GH_PAT=") }) {
		t.Errorf("undeclared GH_PAT leaked to child env %v", got)
	}
}

// Scenario: An undeclared credential is withheld.
func TestChildEnv_WithholdsUndeclaredCredentials(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/alex",
		"GH_PAT=secret-pat", "DD_ACCESS_TOKEN=secret-dd", "LITELLM_KEY=secret-litellm",
	}
	flint := Backend{Name: "flint", Command: "npx"}

	got := flint.ChildEnv(parent)

	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if !isBaseKey(k) {
			t.Errorf("backend declaring no env and no env_passthrough received undeclared variable %q", kv)
		}
	}
	for _, leak := range []string{"GH_PAT", "DD_ACCESS_TOKEN", "LITELLM_KEY"} {
		for _, kv := range got {
			if strings.HasPrefix(kv, leak+"=") {
				t.Errorf("leaked %s to child: %q", leak, kv)
			}
		}
	}
}

// Scenario: The environment is never left unset. nil means "inherit everything" to
// exec.Cmd, which is exactly the failure mode this package exists to prevent.
func TestChildEnv_NeverReturnsNil(t *testing.T) {
	got := Backend{Name: "flint", Command: "npx"}.ChildEnv([]string{"PATH=/usr/bin", "GH_PAT=secret"})
	if got == nil {
		t.Fatal("ChildEnv returned nil; exec.Cmd would inherit the full environment")
	}
}

// Scenario (client-wiring spec, "Committed example configuration carries no internal
// hostnames"): the example config is safe to publish.
func TestLoad_ExampleConfigHasNoInternalHostnames(t *testing.T) {
	cfg, err := Load("../../testdata/config.example.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Backends) == 0 {
		t.Fatal("expected at least one backend")
	}
	for name, b := range cfg.Backends {
		if strings.Contains(b.HTTPURL, "art-internal.com") {
			t.Errorf("backend %q http_url leaks an internal hostname: %q", name, b.HTTPURL)
		}
		for hk, hv := range b.Headers {
			if strings.Contains(hv, "art-internal.com") {
				t.Errorf("backend %q header %q leaks an internal hostname: %q", name, hk, hv)
			}
		}
	}

	github, ok := cfg.Backends["github"]
	if !ok {
		t.Fatal("expected a github backend")
	}
	if auth := github.Headers["Authorization"]; !strings.Contains(auth, "${GH_PAT}") {
		t.Errorf("github backend must reference ${GH_PAT}, got %q", auth)
	}
}

// A bare "*" strips to an empty prefix, which strings.HasPrefix matches against every
// key: that pattern would hand the child the daemon's entire environment. Load must
// reject it rather than silently honouring a blanket grant.
func TestLoad_RejectsBlanketEnvPassthrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"backends": {"art": {"command": "art-mcp-server", "env_passthrough": ["*"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an env_passthrough of \"*\", which would grant the entire environment")
	}
}
