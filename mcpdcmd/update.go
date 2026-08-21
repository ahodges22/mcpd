package mcpdcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ahodges22/mcpd/internal/update"
	"github.com/ahodges22/mcpd/internal/version"
)

type updateOptions struct {
	Version string
	Check   bool
}

type updateRunner interface {
	Check(context.Context) (*update.CheckResult, error)
	Update(context.Context, update.Options) (string, error)
}

func runUpdate(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mcpd executable: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve mcpd executable symlinks: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return runUpdateWith(ctx, args, &update.Updater{
		BinaryPath:     self,
		CurrentVersion: version.String(),
	}, os.Stdout, os.Stderr, runtime.GOOS)
}

func runUpdateWith(ctx context.Context, args []string, updater updateRunner, stdout, stderr io.Writer, goos string) error {
	fs := flag.NewFlagSet("mcpd update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts updateOptions
	fs.StringVar(&opts.Version, "version", "", "install a specific release tag, for example v1.2.3")
	fs.BoolVar(&opts.Check, "check", false, "check for a newer release without installing")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mcpd update [--check] [--version <tag>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("mcpd update accepts no positional arguments")
	}
	if opts.Check && opts.Version != "" {
		return errors.New("--check and --version cannot be used together")
	}
	if opts.Check {
		result, err := updater.Check(ctx)
		if err != nil {
			return err
		}
		if result.Outdated {
			fmt.Fprintf(stdout, "A newer mcpd release is available: %s -> %s\n", result.Current, result.Latest)
			fmt.Fprintln(stdout, "Run `mcpd update` to install it.")
			return nil
		}
		fmt.Fprintf(stdout, "mcpd %s is up to date.\n", result.Current)
		return nil
	}

	fmt.Fprintln(stdout, "Downloading and verifying the mcpd release...")
	tag, err := updater.Update(ctx, update.Options{Version: opts.Version})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Installed %s.\n", tag)
	printRestartGuidance(stdout, goos)
	return nil
}

func printRestartGuidance(output io.Writer, goos string) {
	switch goos {
	case "linux":
		fmt.Fprintln(output, "Restart a supervised daemon with `systemctl --user restart mcpd`.")
	case "darwin":
		fmt.Fprintln(output, "Restart a supervised daemon with `launchctl kickstart -k gui/$(id -u)/dev.mcpd.daemon`.")
	default:
		fmt.Fprintln(output, "Restart any running mcpd daemon to use the new binary.")
	}
}
