// Package config loads mcpd's backend declarations and builds least-privilege
// environments for stdio children.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// baseEnvKeys are forwarded to every stdio child because a process needs them to
// function at all. Everything else must be declared per backend.
var baseEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TERM",
	"TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

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
	Backends map[string]Backend `json:"backends"`
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
			for k, v := range index {
				if strings.HasPrefix(k, prefix) {
					add(k, v)
				}
			}
			continue
		}
		if v, ok := index[pat]; ok {
			add(pat, v)
		}
	}
	for k, v := range b.Env {
		add(k, expand(v, index))
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
		out[k] = expand(v, index)
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

func expand(s string, index map[string]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return index[envRef.FindStringSubmatch(m)[1]]
	})
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(c.Backends) == 0 {
		return nil, fmt.Errorf("config declares no backends")
	}
	for name, b := range c.Backends {
		if b.IsStdio() == (b.HTTPURL != "") {
			return nil, fmt.Errorf("backend %q must declare exactly one of command or http_url", name)
		}
		for _, pat := range b.EnvPassthrough {
			if prefix, ok := strings.CutSuffix(pat, "*"); ok && prefix == "" {
				return nil, fmt.Errorf("backend %q env_passthrough %q would grant its entire environment", name, pat)
			}
		}
		b.Name = name
		c.Backends[name] = b
	}
	return &c, nil
}
