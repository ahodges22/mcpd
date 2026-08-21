package mcpdcmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahodges22/mcpd/internal/config"
	servicepkg "github.com/ahodges22/mcpd/internal/service"
)

type serviceCommandDeps struct {
	executable func() (string, error)
	install    func(servicepkg.Paths) error
	start      func() error
	uninstall  func() error
	inspect    func() (servicepkg.State, error)
	stdout     io.Writer
	stderr     io.Writer
}

func defaultServiceCommandDeps() serviceCommandDeps {
	return serviceCommandDeps{
		executable: os.Executable,
		install:    servicepkg.Install,
		start:      servicepkg.Start,
		uninstall:  servicepkg.Uninstall,
		inspect:    servicepkg.Inspect,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
	}
}

func runService(args []string, deps serviceCommandDeps) error {
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if len(args) == 0 {
		return errors.New("usage: mcpd service <install|start|status|uninstall>")
	}
	switch args[0] {
	case "install":
		return runServiceInstall(args[1:], deps)
	case "start":
		if len(args) != 1 {
			return errors.New("mcpd service start accepts no arguments")
		}
		if err := deps.start(); err != nil {
			return err
		}
		fmt.Fprintln(deps.stdout, "mcpd service started")
		return nil
	case "status":
		if len(args) != 1 {
			return errors.New("mcpd service status accepts no arguments")
		}
		state, err := deps.inspect()
		if err != nil {
			return err
		}
		fmt.Fprintf(deps.stdout, "mcpd service: %s\n", describeServiceState(state))
		return nil
	case "uninstall":
		if len(args) != 1 {
			return errors.New("mcpd service uninstall accepts no arguments")
		}
		if err := deps.uninstall(); err != nil {
			return err
		}
		fmt.Fprintln(deps.stdout, "mcpd service stopped and removed")
		return nil
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func runServiceInstall(args []string, deps serviceCommandDeps) error {
	fs := flag.NewFlagSet("mcpd service install", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultPath("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file")
	statePath := fs.String("state", defaultPath("XDG_STATE_HOME", ".local/state", ""), "state directory")
	addr := fs.String("addr", "127.0.0.1:7420", "address mcpd serves on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("mcpd service install accepts no positional arguments")
	}
	if err := requireLoopbackAddr(*addr); err != nil {
		return err
	}
	binary, err := deps.executable()
	if err != nil {
		return fmt.Errorf("resolve mcpd executable: %w", err)
	}
	paths := servicepkg.Paths{Binary: binary, Config: *configPath, State: *statePath, Addr: *addr}
	for field, value := range map[string]*string{
		"binary": &paths.Binary,
		"config": &paths.Config,
		"state":  &paths.State,
	} {
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return fmt.Errorf("resolve absolute %s path: %w", field, err)
		}
		*value = absolute
	}
	if _, _, err := config.NewWriter(paths.Config); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(paths.State, 0o700); err != nil {
		return fmt.Errorf("restrict state directory: %w", err)
	}
	if err := deps.install(paths); err != nil {
		return err
	}
	fmt.Fprintln(deps.stdout, "mcpd service installed and started")
	return nil
}

func describeServiceState(state servicepkg.State) string {
	if !state.Installed {
		return "not installed"
	}
	parts := []string{"installed"}
	if state.Enabled {
		parts = append(parts, "enabled")
	} else {
		parts = append(parts, "disabled")
	}
	if state.Running {
		parts = append(parts, "running")
	} else {
		parts = append(parts, "stopped")
	}
	return strings.Join(parts, ", ")
}
