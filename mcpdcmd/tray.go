package mcpdcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ahodges22/mcpd/internal/tray"
)

var errTrayLoopExited = errors.New("native tray loop exited without shutdown")

type trayCommandController interface {
	Updates() <-chan tray.MenuModel
	Run(context.Context)
	Repair(tray.MenuCommand, string)
	Retry()
	OpenDashboard()
}

type trayCommandAdapter interface {
	Run(context.Context, <-chan tray.MenuModel) error
}

type trayCommandDeps struct {
	newClient     func(string) (*tray.Client, error)
	newController func(*tray.Client, func(context.Context, string) error) trayCommandController
	newAdapter    func(func(tray.MenuCommand, string)) (trayCommandAdapter, error)
	openURL       func(context.Context, string) error
	signalContext func(context.Context) (context.Context, context.CancelFunc)
}

func defaultTrayCommandDeps() trayCommandDeps {
	return trayCommandDeps{
		newClient: tray.NewClient,
		newController: func(client *tray.Client, openURL func(context.Context, string) error) trayCommandController {
			return tray.NewController(client, openURL)
		},
		newAdapter: func(activate func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
			return tray.NewNativeAdapter(activate)
		},
		openURL: tray.OpenURL,
		signalContext: func(parent context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
		},
	}
}

func runTray(args []string, deps trayCommandDeps) error {
	fs := flag.NewFlagSet("mcpd tray", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "127.0.0.1:7420", "running daemon address")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mcpd tray [--addr <loopback-host:port>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errors.New("mcpd tray accepts no positional arguments")
	}

	client, err := deps.newClient(*addr)
	if err != nil {
		return err
	}
	signalCtx, stopSignals := deps.signalContext(context.Background())
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	controller := deps.newController(client, deps.openURL)
	activate := func(command tray.MenuCommand, backend string) {
		switch command {
		case tray.CommandReconnect, tray.CommandAuthorize:
			controller.Repair(command, backend)
		case tray.CommandRetry:
			controller.Retry()
		case tray.CommandDashboard:
			controller.OpenDashboard()
		case tray.CommandQuit:
			cancel()
		}
	}
	adapter, err := deps.newAdapter(activate)
	if err != nil {
		return err
	}

	controllerDone := make(chan struct{})
	go func() {
		defer close(controllerDone)
		controller.Run(ctx)
	}()

	runErr := adapter.Run(ctx, controller.Updates())
	cleanShutdown := ctx.Err() != nil
	cancel()
	<-controllerDone
	if runErr != nil {
		return fmt.Errorf("run native tray: %w", runErr)
	}
	if cleanShutdown {
		return nil
	}
	return errTrayLoopExited
}
