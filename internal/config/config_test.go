package config

import (
	"log/slog"
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
		"PATH=/usr/bin", "HOME=/home/user",
		"AWS_PROFILE=mgmt", "KUBECONFIG=/home/user/.kube/config",
		"GH_PAT=secret-pat",
	}
	platform := Backend{
		Name:           "platform",
		Command:        "/usr/local/bin/platform-mcp-server",
		EnvPassthrough: []string{"AWS_*", "KUBECONFIG"},
	}

	got := platform.ChildEnv(parent)

	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/home/user",
		"AWS_PROFILE=mgmt", "KUBECONFIG=/home/user/.kube/config",
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
		"PATH=/usr/bin", "HOME=/home/user",
		"GH_PAT=secret-pat", "DD_ACCESS_TOKEN=secret-dd", "GATEWAY_TOKEN=secret-gateway",
	}
	flint := Backend{Name: "flint", Command: "npx"}

	got := flint.ChildEnv(parent)

	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if !isBaseKey(k) {
			t.Errorf("backend declaring no env and no env_passthrough received undeclared variable %q", kv)
		}
	}
	for _, leak := range []string{"GH_PAT", "DD_ACCESS_TOKEN", "GATEWAY_TOKEN"} {
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
		if strings.Contains(b.HTTPURL, "://example.internal") {
			t.Errorf("backend %q http_url leaks an internal hostname: %q", name, b.HTTPURL)
		}
		for hk, hv := range b.Headers {
			if strings.Contains(hv, "://example.internal") {
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// Scenario (backend-management spec, "A backend name is validated on every path that can
// supply one"): a name becomes a URL path segment, a state file name and a tool-id
// prefix, so Load rejects one that could escape any of them. Until backends could be
// declared over HTTP a name was trusted because only a hand-edited file supplied it.
func TestLoad_ValidatesBackendName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"platform", true},
		{"datadog-mcp", true},
		{"knowledge_base", true},
		{"a", true},
		{"x/../../etc/passwd", false},
		{"..", false},
		{"a/b", false},
		{"Platform", false},
		{"-platform", false},
		{"_platform", false},
		{"", false},
		{strings.Repeat("a", 65), false},
	} {
		body := `{"backends": {"` + tc.name + `": {"command": "x"}}}`
		_, err := Load(writeConfig(t, body))
		if tc.ok && err != nil {
			t.Errorf("Load rejected valid name %q: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Load accepted invalid name %q", tc.name)
		}
	}
}

// Scenario (backend-management spec, "Removing the last backend leaves a loadable file"):
// an empty set is legal now that removal can produce one, but a missing or null object
// still is not, because those are what a malformed hand edit looks like and booting with
// every backend silently absent is worse than refusing.
func TestLoad_BackendsMustBePresentButMayBeEmpty(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"backends": {}}`, true},
		{`{"backends": {"platform": {"command": "x"}}}`, true},
		{`{}`, false},
		{`{"backends": null}`, false},
		{`null`, false},
	} {
		_, err := Load(writeConfig(t, tc.body))
		if tc.ok && err != nil {
			t.Errorf("Load rejected %s: %v", tc.body, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Load accepted %s, which declares no backends object", tc.body)
		}
	}
}

// A bare "*" strips to an empty prefix, which strings.HasPrefix matches against every
// key: that pattern would hand the child the daemon's entire environment. Load must
// reject it rather than silently honouring a blanket grant.
func TestLoad_RejectsBlanketEnvPassthrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"backends": {"platform": {"command": "platform-mcp-server", "env_passthrough": ["*"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an env_passthrough of \"*\", which would grant the entire environment")
	}
}

// Scenario: The documented top failure mode, a declaration referencing a variable the
// daemon does not hold, must be named in the log rather than silently expanding to "".
func TestExpansion_WarnsOnUnsetVariables(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	github := Backend{
		Name:           "github",
		HTTPURL:        "https://example.test/mcp",
		Headers:        map[string]string{"Authorization": "Bearer ${GH_TOKEN}"},
		EnvPassthrough: []string{"MISSING_EXACT", "MISSING_PREFIX_*"},
	}
	parent := []string{"PATH=/usr/bin"}

	if got := github.ExpandHeaders(parent)["Authorization"]; got != "Bearer " {
		t.Fatalf("ExpandHeaders resolved %q, want empty expansion", got)
	}
	github.ChildEnv(parent)

	for _, variable := range []string{"GH_TOKEN", "MISSING_EXACT", "MISSING_PREFIX_*"} {
		if !strings.Contains(buf.String(), "variable="+variable) || !strings.Contains(buf.String(), "backend=github") {
			t.Errorf("no warning naming backend github and variable %s; log:\n%s", variable, buf.String())
		}
	}
}

func TestSecretsConfigIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		secrets  string
		wantNil  bool
		provider SecretProvider
		wantErr  bool
	}{
		{name: "omitted", wantNil: true},
		{name: "native", secrets: `,"secrets":{"provider":"native"}`, provider: SecretProviderNative},
		{name: "file", secrets: `,"secrets":{"provider":"file"}`, provider: SecretProviderFile},
		{name: "empty block", secrets: `,"secrets":{}`, wantErr: true},
		{name: "unknown provider", secrets: `,"secrets":{"provider":"vault"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, `{"backends":{}`+tc.secrets+`}`))
			if tc.wantErr {
				if err == nil {
					t.Fatal("Load accepted an invalid secrets block")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.wantNil {
				if cfg.Secrets != nil {
					t.Fatalf("omitted secrets block became %#v", cfg.Secrets)
				}
				return
			}
			if cfg.Secrets == nil || cfg.Secrets.Provider != tc.provider {
				t.Fatalf("provider = %#v, want %q", cfg.Secrets, tc.provider)
			}
		})
	}
}

func TestSecretReferencesAreAllowlisted(t *testing.T) {
	cfg := Config{
		Backends: map[string]Backend{
			"stdio": {
				Name:    "stdio",
				Command: "${COMMAND_SECRET}",
				Args:    []string{"${ARG_SECRET}"},
				Env: map[string]string{
					"TOKEN": "prefix-${STDIO_TOKEN}-${SHARED}",
				},
				Headers: map[string]string{"X-Unused": "${UNUSED_STDIO_HEADER}"},
			},
			"http": {
				Name:    "http",
				HTTPURL: "https://${URL_SECRET}/mcp",
				Env:     map[string]string{"UNUSED": "${UNUSED_HTTP_ENV}"},
				Headers: map[string]string{
					"Authorization": "Bearer ${HTTP_TOKEN}",
					"X-Shared":      "${SHARED}",
				},
			},
		},
		Embeddings: Embeddings{URL: "https://example.test", APIKeyEnv: "EMBEDDINGS_TOKEN"},
		Remote:     Remote{Advertise: "https://${REMOTE_SECRET}"},
		Secrets:    &Secrets{Provider: SecretProviderNative},
	}

	got := cfg.SecretConsumers()
	want := []SecretConsumer{
		{Kind: ConsumerBackend, Name: "http", References: []string{"HTTP_TOKEN", "SHARED"}},
		{Kind: ConsumerBackend, Name: "stdio", References: []string{"SHARED", "STDIO_TOKEN"}},
		{Kind: ConsumerEmbeddings, Name: "embeddings", References: []string{"EMBEDDINGS_TOKEN"}},
	}
	if !slices.EqualFunc(got, want, func(a, b SecretConsumer) bool {
		return a.Kind == b.Kind && a.Name == b.Name && slices.Equal(a.References, b.References)
	}) {
		t.Fatalf("SecretConsumers() = %#v, want %#v", got, want)
	}
	for _, forbidden := range []string{
		"COMMAND_SECRET", "ARG_SECRET", "URL_SECRET", "REMOTE_SECRET",
		"UNUSED_STDIO_HEADER", "UNUSED_HTTP_ENV",
	} {
		for _, consumer := range got {
			if slices.Contains(consumer.References, forbidden) {
				t.Errorf("non-allowlisted reference %q was inventoried", forbidden)
			}
		}
	}
}

func TestEnvironmentPresenceWins(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	b := Backend{
		Name:    "github",
		Command: "server",
		Env:     map[string]string{"TOKEN": "${TOKEN}"},
		Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"},
	}
	parent := []string{"PATH=/usr/bin", "TOKEN="}

	if got := b.ExpandHeaders(parent)["Authorization"]; got != "Bearer " {
		t.Fatalf("header = %q, want present empty environment value", got)
	}
	if got := b.ChildEnv(parent); !slices.Contains(got, "TOKEN=") {
		t.Fatalf("child environment does not contain present empty TOKEN: %v", got)
	}
	if strings.Contains(logs.String(), "variable=TOKEN") {
		t.Fatalf("present empty TOKEN was reported missing: %s", logs.String())
	}
}
