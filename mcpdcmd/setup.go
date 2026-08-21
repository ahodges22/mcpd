package mcpdcmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ahodges22/mcpd/internal/install"
)

type setupCommandDeps struct {
	homeDir        func() (string, error)
	installService func(configPath, statePath, addr string) error
	doctor         func(configPath, addr string) error
	stdin          io.Reader
	stdout         io.Writer
	stderr         io.Writer
}

func defaultSetupCommandDeps() setupCommandDeps {
	return setupCommandDeps{
		homeDir: os.UserHomeDir,
		installService: func(configPath, statePath, addr string) error {
			return runService([]string{"install", "--config", configPath, "--state", statePath, "--addr", addr}, defaultServiceCommandDeps())
		},
		doctor: func(configPath, addr string) error {
			return runDoctor([]string{"--config", configPath, "--addr", addr}, defaultDoctorCommandDeps())
		},
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
}

type setupPlan struct {
	client install.Client
	plan   install.Plan
}

func runSetup(args []string, deps setupCommandDeps) error {
	if deps.stdin == nil {
		deps.stdin = strings.NewReader("")
	}
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	fs := flag.NewFlagSet("mcpd setup", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultPath("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file")
	statePath := fs.String("state", defaultPath("XDG_STATE_HOME", ".local/state", ""), "state directory")
	addr := fs.String("addr", "127.0.0.1:7420", "address mcpd serves on")
	yes := fs.Bool("yes", false, "apply detected client changes without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("mcpd setup accepts no positional arguments")
	}
	if err := requireLoopbackAddr(*addr); err != nil {
		return err
	}
	if err := deps.installService(*configPath, *statePath, *addr); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	if err := deps.doctor(*configPath, *addr); err != nil {
		return fmt.Errorf("verify installation: %w", err)
	}
	home, err := deps.homeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	plans, err := setupPlans(home, *statePath, *addr, deps.stdout)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		fmt.Fprintln(deps.stdout, "No client configuration changes are needed.")
		fmt.Fprintf(deps.stdout, "mcpd is running at http://%s\n", *addr)
		return nil
	}
	if !*yes {
		fmt.Fprint(deps.stdout, "Apply these client changes? [y/N] ")
		line, readErr := bufio.NewReader(deps.stdin).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read setup confirmation: %w", readErr)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(deps.stdout, "Client configurations were not changed.")
			return nil
		}
	}
	for _, planned := range plans {
		if err := planned.client.Apply(*statePath, planned.plan); err != nil {
			return fmt.Errorf("install %s: %w", planned.client.Name, err)
		}
		fmt.Fprintf(deps.stdout, "%s: configured; restart the client to pick it up\n", planned.client.Name)
	}
	fmt.Fprintf(deps.stdout, "mcpd is running at http://%s\n", *addr)
	return nil
}

func setupPlans(home, state, addr string, output io.Writer) ([]setupPlan, error) {
	var plans []setupPlan
	for _, client := range install.Clients(home) {
		info, err := os.Stat(client.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", client.Path, err)
		}
		if info.IsDir() {
			continue
		}
		fmt.Fprintf(output, "Detected client: %s (%s)\n", client.Name, client.Path)
		installed, err := client.Installed(state)
		if err != nil {
			return nil, err
		}
		if installed {
			fmt.Fprintln(output, "  - already configured")
			continue
		}
		plan, err := planFor(client, state, addr, false)
		if err != nil {
			return nil, err
		}
		for _, note := range plan.Notes {
			fmt.Fprintln(output, "  - "+note)
		}
		for _, warning := range plan.Warnings {
			fmt.Fprintln(output, "  ! "+warning)
		}
		if client.Mode == install.Search {
			fmt.Fprintln(output, "  ! this client uses one approval decision for all upstream tools through call_tool")
		}
		if !plan.Empty() {
			plans = append(plans, setupPlan{client: client, plan: plan})
		}
	}
	return plans, nil
}
