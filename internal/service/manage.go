package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type State struct {
	Installed bool
	Enabled   bool
	Running   bool
}

func Inspect() (State, error) {
	switch platform {
	case "linux":
		output, err := runner("systemctl", "--user", "show", "mcpd.service", "--property=LoadState", "--property=UnitFileState", "--property=ActiveState")
		if err != nil {
			return State{}, err
		}
		return parseSystemdState(output)
	case "darwin":
		return inspectLaunchd()
	default:
		return State{}, fmt.Errorf("service status is not supported on %s", platform)
	}
}

func Start() error {
	switch platform {
	case "linux":
		_, err := runner("systemctl", "--user", "start", "mcpd.service")
		return err
	case "darwin":
		state, err := inspectLaunchd()
		if err != nil {
			return err
		}
		domain := "gui/" + userID()
		if state.Running {
			_, err = runner("launchctl", "kickstart", "-k", domain+"/dev.mcpd.daemon")
			return err
		}
		path, err := launchdPath()
		if err != nil {
			return err
		}
		_, err = runner("launchctl", "bootstrap", domain, path)
		return err
	default:
		return fmt.Errorf("service start is not supported on %s", platform)
	}
}

func Uninstall() error {
	switch platform {
	case "linux":
		return uninstallSystemd()
	case "darwin":
		return uninstallLaunchd()
	default:
		return fmt.Errorf("service uninstall is not supported on %s", platform)
	}
}

func uninstallSystemd() error {
	path, err := systemdPath()
	if err != nil {
		return err
	}
	stopOutput, stopErr := runner("systemctl", "--user", "disable", "--now", "mcpd.service")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if _, err := runner("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if stopErr != nil && !notLoaded(fmt.Errorf("%s: %w", stopOutput, stopErr)) {
		return fmt.Errorf("service file removed, but stopping the service failed (it may still be running): %w", stopErr)
	}
	return nil
}

func uninstallLaunchd() error {
	path, err := launchdPath()
	if err != nil {
		return err
	}
	_, stopErr := runner("launchctl", "bootout", "gui/"+userID()+"/dev.mcpd.daemon")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if stopErr != nil && !notLoaded(stopErr) {
		return fmt.Errorf("service file removed, but stopping the service failed (it may still be running): %w", stopErr)
	}
	return nil
}

func parseSystemdState(output string) (State, error) {
	values := make(map[string]string, 3)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	load, hasLoad := values["LoadState"]
	active, hasActive := values["ActiveState"]
	if !hasLoad || !hasActive {
		return State{}, errors.New("systemctl returned an unrecognized service state")
	}
	unit := values["UnitFileState"]
	return State{
		Installed: load == "loaded",
		Enabled:   unit == "enabled" || unit == "enabled-runtime",
		Running:   active == "active",
	}, nil
}

func inspectLaunchd() (State, error) {
	path, err := launchdPath()
	if err != nil {
		return State{}, err
	}
	_, statErr := os.Stat(path)
	installed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return State{}, statErr
	}
	output, printErr := runner("launchctl", "print", "gui/"+userID()+"/dev.mcpd.daemon")
	if printErr == nil {
		return State{Installed: installed, Enabled: installed, Running: launchdRunning(output)}, nil
	}
	if notLoaded(printErr) {
		return State{Installed: installed, Enabled: installed}, nil
	}
	return State{}, printErr
}

func launchdRunning(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if state, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
			return state == "running"
		}
	}
	return false
}

func launchdPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", "dev.mcpd.daemon.plist"), nil
}
