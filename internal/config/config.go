// Package config loads mcpd's backend declarations and builds least-privilege
// environments for stdio children.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

// baseEnvKeys are forwarded to every stdio child because a process needs them to
// function at all. Everything else must be declared per backend.
var baseEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM",
	"TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// nameRef bounds a backend name because the name becomes a URL path segment, a file
// name under the state directory, and the prefix of every canonical tool id. A name
// arriving over HTTP is not trusted, and the file path shares this validator so the
// two cannot drift.
var nameRef = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidName(name string) bool { return nameRef.MatchString(name) }

type Backend struct {
	Name           string            `json:"-"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
	HTTPURL        string            `json:"http_url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Auth           string            `json:"auth,omitempty"` // "" or "oauth"
	TimeoutSec     int               `json:"timeout,omitempty"`
}

type Config struct {
	Backends   map[string]Backend `json:"backends"`
	Embeddings Embeddings         `json:"embeddings,omitempty"`
	Ranking    Ranking            `json:"ranking,omitempty"`
	Remote     Remote             `json:"remote,omitempty"`
}

// Remote declares the opt-in LAN relogin listener. Only the declaration lives
// here: the pairing token is a credential and lives in the state directory.
type Remote struct {
	Enabled bool   `json:"enabled,omitempty"`
	Addr    string `json:"addr,omitempty"`
	// Advertise is the origin a reverse proxy serves the listener on. It leads
	// the pairing URLs and names the host in the relogin page's instructions.
	Advertise string `json:"advertise,omitempty"`
}

// Embeddings configures the gateway that vectorizes the catalog. It is optional: with no
// URL, ranking degrades to lexical only and abstention stays inert, which is a worse
// search rather than a broken daemon.
type Embeddings struct {
	URL   string `json:"url,omitempty"`
	Model string `json:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the key, rather than the key
	// itself: the declaration file is not where a credential belongs, and every other
	// secret in it is already a ${VAR} reference.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Enabled reports whether a gateway is configured at all.
func (e Embeddings) Enabled() bool { return e.URL != "" }

// APIKey resolves the key from the environment the daemon itself holds.
func (e Embeddings) APIKey() string {
	if e.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(e.APIKeyEnv)
}

type Ranking struct {
	ExpansionModel  string `json:"expansion_model,omitempty"`
	RerankModel     string `json:"rerank_model,omitempty"`
	RerankTimeoutMS int    `json:"rerank_timeout_ms,omitempty"`
}

func (r Ranking) Enabled() bool { return r.ExpansionModel != "" && r.RerankModel != "" }

func (r Ranking) RerankTimeout() time.Duration {
	return time.Duration(r.RerankTimeoutMS) * time.Millisecond
}

func (b Backend) IsStdio() bool { return b.Command != "" }

// ChildEnv builds the environment for a stdio child: a curated base, plus the
// backend's declared env, plus its declared passthrough patterns. It never returns
// nil, because exec.Cmd treats nil as "inherit everything".
func (b Backend) ChildEnv(parent []string) []string {
	index := indexEnv(parent)
	out := make([]string, 0, len(baseEnvKeys)+len(b.Env)+len(b.EnvPassthrough))
	add := func(k, v string) { out = append(out, k+"="+v) }

	for _, k := range baseEnvKeys {
		if v, ok := index[k]; ok {
			add(k, v)
		}
	}
	for _, pat := range b.EnvPassthrough {
		if prefix, ok := strings.CutSuffix(pat, "*"); ok {
			matched := false
			for k, v := range index {
				if strings.HasPrefix(k, prefix) {
					add(k, v)
					matched = true
				}
			}
			if !matched {
				b.warnUnset("env_passthrough matches nothing in the daemon environment", pat)
			}
			continue
		}
		if v, ok := index[pat]; ok {
			add(pat, v)
		} else {
			b.warnUnset("env_passthrough variable is not set in the daemon environment", pat)
		}
	}
	for k, v := range b.Env {
		add(k, b.expand(v, index))
	}
	return out
}

// ExpandHeaders resolves ${VAR} references in HTTP headers against the daemon's own
// environment. Unlike ChildEnv this is not a privilege boundary: the value is used by
// mcpd itself, not handed to a child process.
func (b Backend) ExpandHeaders(parent []string) map[string]string {
	index := indexEnv(parent)
	out := make(map[string]string, len(b.Headers))
	for k, v := range b.Headers {
		out[k] = b.expand(v, index)
	}
	return out
}

func indexEnv(parent []string) map[string]string {
	index := make(map[string]string, len(parent))
	for _, kv := range parent {
		if k, v, ok := strings.Cut(kv, "="); ok {
			index[k] = v
		}
	}
	return index
}

// expand resolves ${VAR} references, warning on any variable the daemon does not
// hold: the reference still becomes "", but silently sending "Bearer " upstream
// looks exactly like a bad credential, so the log must name the real cause.
func (b Backend) expand(s string, index map[string]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := index[name]
		if !ok {
			b.warnUnset("declaration references a variable the daemon environment does not hold", name)
		}
		return v
	})
}

func (b Backend) warnUnset(msg, variable string) {
	slog.Warn(msg, "backend", b.Name, "variable", variable)
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// parse holds the shared validation, so a file loaded at startup and a document
	// written through the panel are held to exactly the same rules.
	return parse(raw)
}

func validateRanking(c Config) error {
	configured := c.Ranking.ExpansionModel != "" || c.Ranking.RerankModel != "" || c.Ranking.RerankTimeoutMS != 0
	if !configured {
		return nil
	}
	if !c.Embeddings.Enabled() {
		return fmt.Errorf("ranking requires an embeddings gateway")
	}
	if !c.Ranking.Enabled() {
		return fmt.Errorf("ranking requires both expansion_model and rerank_model")
	}
	if c.Ranking.RerankTimeoutMS <= 0 {
		return fmt.Errorf("ranking rerank_timeout_ms must be positive")
	}
	return nil
}
