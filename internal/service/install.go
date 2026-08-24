// Package service installs and manages mcpd as a per-user service.
package service

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/ahodges22/mcpd/internal/atomicfile"
)

//go:embed mcpd.service.tmpl
var systemdTemplate string

//go:embed dev.mcpd.daemon.plist.tmpl
var launchdTemplate string

var platform = runtime.GOOS
var runner = run

type Paths struct {
	Binary          string
	Config          string
	State           string
	Addr            string
	PassEnvironment []string
}

type launchdTemplateData struct {
	Paths
	Log string
}

func RenderSystemdUnit(paths Paths) (string, error) {
	if err := validatePaths(paths); err != nil {
		return "", err
	}
	passEnvironment, err := normalizePassEnvironment(paths.PassEnvironment)
	if err != nil {
		return "", err
	}
	paths.PassEnvironment = passEnvironment
	tmpl, err := template.New("mcpd.service").Parse(systemdTemplate)
	if err != nil {
		return "", err
	}
	paths.Binary = systemdEscape(paths.Binary)
	paths.Config = systemdEscape(paths.Config)
	paths.State = systemdEscape(paths.State)
	paths.Addr = systemdEscape(paths.Addr)
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, paths); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

func normalizePassEnvironment(names []string) ([]string, error) {
	unique := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" || strings.IndexFunc(name, func(r rune) bool {
			return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
		}) >= 0 {
			return nil, fmt.Errorf("environment variable %q contains an unsafe character", name)
		}
		unique[name] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for name := range unique {
		normalized = append(normalized, name)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func RenderLaunchdPlist(paths Paths, logPath string) (string, error) {
	if err := validatePaths(paths); err != nil {
		return "", err
	}
	if err := validatePath("log", logPath); err != nil {
		return "", err
	}
	tmpl, err := template.New("dev.mcpd.daemon.plist").Parse(launchdTemplate)
	if err != nil {
		return "", err
	}
	data := launchdTemplateData{
		Paths: Paths{
			Binary: xmlEscape(paths.Binary),
			Config: xmlEscape(paths.Config),
			State:  xmlEscape(paths.State),
			Addr:   xmlEscape(paths.Addr),
		},
		Log: xmlEscape(logPath),
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

func Install(paths Paths) error {
	switch platform {
	case "linux":
		return installSystemd(paths)
	case "darwin":
		return installLaunchd(paths)
	default:
		return fmt.Errorf("service install is not supported on %s", platform)
	}
}

func installSystemd(paths Paths) error {
	if _, err := runner("systemctl", "--user", "is-active", "--quiet", "graphical-session.target"); err != nil {
		return fmt.Errorf("graphical session is not active; mcpd will not start outside its credential-bearing session: %w", err)
	}
	unit, err := RenderSystemdUnit(paths)
	if err != nil {
		return fmt.Errorf("render systemd unit: %w", err)
	}
	path, err := systemdPath()
	if err != nil {
		return err
	}
	if err := writeServiceFile(path, unit); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "mcpd.service"},
		{"--user", "restart", "mcpd.service"},
	} {
		if _, err := runner("systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

func installLaunchd(paths Paths) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	logPath := filepath.Join(home, "Library", "Logs", "mcpd.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create launchd log directory: %w", err)
	}
	plist, err := RenderLaunchdPlist(paths, logPath)
	if err != nil {
		return fmt.Errorf("render launchd plist: %w", err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", "dev.mcpd.daemon.plist")
	if err := writeServiceFile(path, plist); err != nil {
		return err
	}
	domain := "gui/" + userID()
	target := domain + "/dev.mcpd.daemon"
	if _, err := runner("launchctl", "bootout", target); err != nil && !notLoaded(err) {
		return err
	}
	if err := waitForLaunchdUnload(target); err != nil {
		return err
	}
	if _, err := runner("launchctl", "enable", target); err != nil {
		return err
	}
	_, err = runner("launchctl", "bootstrap", domain, path)
	return err
}

func waitForLaunchdUnload(target string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := runner("launchctl", "print", target); err != nil {
			if notLoaded(err) {
				return nil
			}
			return fmt.Errorf("inspect launchd service %s: %w", target, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for launchd service %s to unload", target)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func systemdPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", "mcpd.service"), nil
}

func writeServiceFile(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create service directory: %w", err)
	}
	if err := atomicfile.Write(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func userID() string { return strconv.Itoa(os.Getuid()) }

func notLoaded(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such process") ||
		strings.Contains(message, "could not find service") ||
		strings.Contains(message, "service not found") ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "unit not found")
}

func run(name string, args ...string) (string, error) {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed: %w: %s", name, err, bytes.TrimSpace(output))
	}
	return string(output), nil
}

func systemdEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "$", "$$")
	return strings.ReplaceAll(value, "%", "%%")
}

func validatePaths(paths Paths) error {
	for name, value := range map[string]string{
		"binary": paths.Binary,
		"config": paths.Config,
		"state":  paths.State,
	} {
		if err := validatePath(name, value); err != nil {
			return err
		}
	}
	if paths.Addr == "" {
		return fmt.Errorf("address is empty")
	}
	if strings.IndexFunc(paths.Addr, unicode.IsControl) >= 0 {
		return fmt.Errorf("address contains a control character")
	}
	return nil
}

func validatePath(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s path is empty", name)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s path contains a control character", name)
	}
	return nil
}

func xmlEscape(value string) string {
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		panic(fmt.Sprintf("escape XML text: %v", err))
	}
	return escaped.String()
}
