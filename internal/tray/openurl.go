package tray

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

type browserCommandRunner func(context.Context, string, ...string) error

func OpenURL(ctx context.Context, raw string) error {
	return openURLWith(ctx, raw, runtime.GOOS, func(ctx context.Context, executable string, args ...string) error {
		return exec.CommandContext(ctx, executable, args...).Run()
	})
}

func openURLWith(ctx context.Context, raw, goos string, run browserCommandRunner) error {
	if err := validateAuthorizeURL(raw); err != nil {
		return fmt.Errorf("open browser URL: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("open browser URL: %w", err)
	}

	var executable string
	switch goos {
	case "darwin":
		executable = "/usr/bin/open"
	case "linux":
		executable = "xdg-open"
	default:
		return fmt.Errorf("open browser URL: unsupported platform %q", goos)
	}
	if run == nil {
		return fmt.Errorf("open browser URL: no command runner configured")
	}
	if err := run(ctx, executable, raw); err != nil {
		return fmt.Errorf("open browser URL: %w", err)
	}
	return nil
}
