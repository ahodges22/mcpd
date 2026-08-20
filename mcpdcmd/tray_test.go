package mcpdcmd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/tray"
)

type fakeTrayController struct {
	updates    chan tray.MenuModel
	started    chan struct{}
	stopped    chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	repairs    chan recordedTrayAction
	retries    atomic.Int32
	dashboards atomic.Int32
}

type recordedTrayAction struct {
	command tray.MenuCommand
	backend string
}

func newFakeTrayController() *fakeTrayController {
	return &fakeTrayController{
		updates: make(chan tray.MenuModel),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
		repairs: make(chan recordedTrayAction, 4),
	}
}

func (c *fakeTrayController) Updates() <-chan tray.MenuModel { return c.updates }

func (c *fakeTrayController) Run(ctx context.Context) {
	c.startOnce.Do(func() { close(c.started) })
	<-ctx.Done()
	c.stopOnce.Do(func() {
		close(c.updates)
		close(c.stopped)
	})
}

func (c *fakeTrayController) Repair(command tray.MenuCommand, backend string) {
	c.repairs <- recordedTrayAction{command: command, backend: backend}
}

func (c *fakeTrayController) Retry()         { c.retries.Add(1) }
func (c *fakeTrayController) OpenDashboard() { c.dashboards.Add(1) }

type fakeTrayAdapter struct {
	run func(context.Context, <-chan tray.MenuModel) error
}

func (a fakeTrayAdapter) Run(ctx context.Context, updates <-chan tray.MenuModel) error {
	return a.run(ctx, updates)
}

func testTrayCommandDeps(controller *fakeTrayController, adapter trayCommandAdapter) trayCommandDeps {
	return trayCommandDeps{
		newClient: tray.NewClient,
		newController: func(*tray.Client, func(context.Context, string) error) trayCommandController {
			return controller
		},
		newAdapter: func(func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
			return adapter, nil
		},
		openURL: tray.OpenURL,
		signalContext: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(parent)
		},
	}
}

func preCancelledSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	return ctx, func() {}
}

func TestRunTrayFlags(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantAddr string
	}{
		{name: "default", wantAddr: "127.0.0.1:7420"},
		{name: "custom", args: []string{"--addr", "[::1]:8123"}, wantAddr: "[::1]:8123"},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller := newFakeTrayController()
			adapter := fakeTrayAdapter{run: func(ctx context.Context, _ <-chan tray.MenuModel) error {
				<-ctx.Done()
				return nil
			}}
			deps := testTrayCommandDeps(controller, adapter)
			deps.signalContext = preCancelledSignalContext
			var gotAddr string
			deps.newClient = func(addr string) (*tray.Client, error) {
				gotAddr = addr
				return tray.NewClient(addr)
			}
			if err := runTray(test.args, deps); err != nil {
				t.Fatalf("runTray: %v", err)
			}
			if gotAddr != test.wantAddr {
				t.Fatalf("address = %q, want %q", gotAddr, test.wantAddr)
			}
		})
	}

	for _, args := range [][]string{{"extra"}, {"--unknown"}, {"--addr"}} {
		t.Run("rejects invalid arguments", func(t *testing.T) {
			controller := newFakeTrayController()
			adapter := fakeTrayAdapter{run: func(context.Context, <-chan tray.MenuModel) error { return nil }}
			deps := testTrayCommandDeps(controller, adapter)
			clientCalled := false
			deps.newClient = func(string) (*tray.Client, error) {
				clientCalled = true
				return nil, nil
			}
			if err := runTray(args, deps); err == nil {
				t.Fatalf("runTray(%q) succeeded", args)
			}
			if clientCalled {
				t.Fatal("invalid flags constructed a client")
			}
		})
	}
}

func TestRunTrayRejectsNonLoopback(t *testing.T) {
	controller := newFakeTrayController()
	adapterConstructed := false
	deps := testTrayCommandDeps(controller, fakeTrayAdapter{})
	deps.newAdapter = func(func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
		adapterConstructed = true
		return nil, nil
	}
	if err := runTray([]string{"--addr", "0.0.0.0:7420"}, deps); err == nil {
		t.Fatal("runTray accepted a non-loopback address")
	}
	if adapterConstructed {
		t.Fatal("non-loopback address reached native construction")
	}
}

func TestRunTrayExitOutcome(t *testing.T) {
	t.Run("quit is clean", func(t *testing.T) {
		controller := newFakeTrayController()
		var activate func(tray.MenuCommand, string)
		deps := testTrayCommandDeps(controller, fakeTrayAdapter{run: func(ctx context.Context, _ <-chan tray.MenuModel) error {
			activate(tray.CommandQuit, "")
			<-ctx.Done()
			return nil
		}})
		deps.newAdapter = func(callback func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
			activate = callback
			return fakeTrayAdapter{run: func(ctx context.Context, _ <-chan tray.MenuModel) error {
				callback(tray.CommandQuit, "")
				<-ctx.Done()
				return nil
			}}, nil
		}
		if err := runTray(nil, deps); err != nil {
			t.Fatalf("runTray: %v", err)
		}
		select {
		case <-controller.stopped:
		default:
			t.Fatal("runTray returned before controller shutdown")
		}
	})

	t.Run("signal is clean", func(t *testing.T) {
		controller := newFakeTrayController()
		adapter := fakeTrayAdapter{run: func(ctx context.Context, _ <-chan tray.MenuModel) error {
			<-ctx.Done()
			return nil
		}}
		deps := testTrayCommandDeps(controller, adapter)
		deps.signalContext = preCancelledSignalContext
		if err := runTray(nil, deps); err != nil {
			t.Fatalf("runTray: %v", err)
		}
	})

	t.Run("native error is preserved", func(t *testing.T) {
		wantErr := errors.New("native loop failed")
		controller := newFakeTrayController()
		deps := testTrayCommandDeps(controller, fakeTrayAdapter{run: func(context.Context, <-chan tray.MenuModel) error {
			return wantErr
		}})
		if err := runTray(nil, deps); !errors.Is(err, wantErr) {
			t.Fatalf("runTray error = %v, want %v", err, wantErr)
		}
		select {
		case <-controller.stopped:
		default:
			t.Fatal("native error returned before controller shutdown")
		}
	})

	t.Run("unexpected nil native exit is a failure", func(t *testing.T) {
		controller := newFakeTrayController()
		deps := testTrayCommandDeps(controller, fakeTrayAdapter{run: func(context.Context, <-chan tray.MenuModel) error {
			return nil
		}})
		if err := runTray(nil, deps); !errors.Is(err, errTrayLoopExited) {
			t.Fatalf("runTray error = %v, want %v", err, errTrayLoopExited)
		}
	})

	t.Run("construction failures precede controller start", func(t *testing.T) {
		wantClientErr := errors.New("client construction")
		controller := newFakeTrayController()
		deps := testTrayCommandDeps(controller, fakeTrayAdapter{})
		deps.newClient = func(string) (*tray.Client, error) { return nil, wantClientErr }
		if err := runTray(nil, deps); !errors.Is(err, wantClientErr) {
			t.Fatalf("client error = %v, want %v", err, wantClientErr)
		}

		wantAdapterErr := errors.New("adapter construction")
		controller = newFakeTrayController()
		deps = testTrayCommandDeps(controller, fakeTrayAdapter{})
		deps.newAdapter = func(func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
			return nil, wantAdapterErr
		}
		if err := runTray(nil, deps); !errors.Is(err, wantAdapterErr) {
			t.Fatalf("adapter error = %v, want %v", err, wantAdapterErr)
		}
		select {
		case <-controller.started:
			t.Fatal("controller started after construction failure")
		default:
		}
	})

	t.Run("callbacks route without native work", func(t *testing.T) {
		controller := newFakeTrayController()
		deps := testTrayCommandDeps(controller, fakeTrayAdapter{})
		deps.newAdapter = func(callback func(tray.MenuCommand, string)) (trayCommandAdapter, error) {
			return fakeTrayAdapter{run: func(ctx context.Context, _ <-chan tray.MenuModel) error {
				callback(tray.CommandReconnect, "alpha")
				callback(tray.CommandAuthorize, "beta")
				callback(tray.CommandRetry, "")
				callback(tray.CommandDashboard, "")
				callback(tray.CommandQuit, "")
				<-ctx.Done()
				return nil
			}}, nil
		}
		if err := runTray(nil, deps); err != nil {
			t.Fatalf("runTray: %v", err)
		}
		wantRepairs := []recordedTrayAction{
			{command: tray.CommandReconnect, backend: "alpha"},
			{command: tray.CommandAuthorize, backend: "beta"},
		}
		for _, want := range wantRepairs {
			select {
			case got := <-controller.repairs:
				if got != want {
					t.Fatalf("repair = %#v, want %#v", got, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("missing repair %#v", want)
			}
		}
		if controller.retries.Load() != 1 || controller.dashboards.Load() != 1 {
			t.Fatalf("retry calls = %d, dashboard calls = %d", controller.retries.Load(), controller.dashboards.Load())
		}
	})
}
