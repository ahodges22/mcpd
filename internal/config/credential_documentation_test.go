package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const credentialGuidePath = "../../docs/credential-providers.md"

func TestCredentialProviderDocumentationExamplesParse(t *testing.T) {
	body, err := os.ReadFile(credentialGuidePath)
	if err != nil {
		t.Fatalf("ReadFile credential guide: %v", err)
	}
	blocks := fencedBlocks(string(body), "json")
	if len(blocks) < 2 {
		t.Fatalf("credential guide has %d JSON examples, want native and file examples", len(blocks))
	}
	wantProviders := map[SecretProvider]bool{SecretProviderNative: false, SecretProviderFile: false}
	for i, block := range blocks {
		cfg, err := Load(writeConfig(t, block))
		if err != nil {
			t.Errorf("JSON example %d does not parse as mcpd configuration: %v", i+1, err)
			continue
		}
		if cfg.Secrets != nil {
			if _, documented := wantProviders[cfg.Secrets.Provider]; documented {
				wantProviders[cfg.Secrets.Provider] = true
			}
		}
	}
	for provider, found := range wantProviders {
		if !found {
			t.Errorf("credential guide has no parseable %q provider example", provider)
		}
	}
}

func TestCredentialProviderDocumentationCoversOperations(t *testing.T) {
	body, err := os.ReadFile(credentialGuidePath)
	if err != nil {
		t.Fatalf("ReadFile credential guide: %v", err)
	}
	for _, phrase := range []string{
		"2048 bytes",
		"present but empty",
		"macOS Keychain",
		"session D-Bus",
		"headless",
		"corrupt",
		"chmod 0700",
		"Migration",
		"Rollback",
	} {
		if !strings.Contains(string(body), phrase) {
			t.Errorf("credential guide omits %q", phrase)
		}
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("ReadFile README: %v", err)
	}
	if !strings.Contains(string(readme), "docs/credential-providers.md") {
		t.Fatal("README does not link the credential provider guide")
	}
}

func TestCredentialDocumentationHasNoSecretLiterals(t *testing.T) {
	tokenPattern := regexp.MustCompile(`\b(?:AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{16,}|xox[baprs]-[A-Za-z0-9-]{16,})\b`)
	paths := []string{"../../README.md", "../../dist/README.md"}
	err := filepath.WalkDir("../../docs", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk documentation: %v", err)
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		text := string(body)
		if match := tokenPattern.FindString(text); match != "" {
			t.Errorf("%s contains a token-shaped literal %q", path, match)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, `"Authorization"`) && strings.Contains(line, "Bearer ") && !strings.Contains(line, "${") {
				t.Errorf("%s contains a literal bearer authorization example: %s", path, line)
			}
			if strings.Contains(line, "mcpd secret set") {
				fields := strings.Fields(line)
				for i := 0; i+3 < len(fields); i++ {
					if fields[i] == "mcpd" && fields[i+1] == "secret" && fields[i+2] == "set" && i+4 < len(fields) {
						t.Errorf("%s supplies a secret value as a command argument: %s", path, line)
					}
				}
			}
		}
		if strings.Contains(text, `"value":`) {
			t.Errorf("%s documents a secret API value literal", path)
		}
	}
}

func fencedBlocks(body, language string) []string {
	marker := "```" + language + "\n"
	var blocks []string
	for {
		_, after, ok := strings.Cut(body, marker)
		if !ok {
			return blocks
		}
		block, rest, ok := strings.Cut(after, "\n```")
		if !ok {
			return blocks
		}
		blocks = append(blocks, block)
		body = rest
	}
}
