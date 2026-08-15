package tray

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type actionTestClient struct {
	status    func(context.Context) (Status, error)
	reconnect func(context.Context, string) error
	authorize func(context.Context, string) (string, error)
}

func (c actionTestClient) Status(ctx context.Context) (Status, error) {
	return c.status(ctx)
}

func (c actionTestClient) Reconnect(ctx context.Context, name string) error {
	return c.reconnect(ctx, name)
}

func (c actionTestClient) Authorize(ctx context.Context, name string) (string, error) {
	return c.authorize(ctx, name)
}

func TestControllerSerializesAction(t *testing.T) {
	status := actionableStatus()
	started := make(chan string, 4)
	release := make(chan struct{}, 2)
	opened := make(chan string, 1)
	var reconnects atomic.Int64
	var authorizations atomic.Int64
	client := actionTestClient{
		status: func(context.Context) (Status, error) {
			return status, nil
		},
		reconnect: func(ctx context.Context, name string) error {
			reconnects.Add(1)
			started <- "reconnect:" + name
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		authorize: func(ctx context.Context, name string) (string, error) {
			authorizations.Add(1)
			started <- "authorize:" + name
			select {
			case <-release:
				return "https://login.example/authorize", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	controller := newActionController(client, func(_ context.Context, target string) error {
		opened <- target
		return nil
	}, make(chan time.Time))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)
	_ = wantModel(t, controller.Updates())

	controller.Repair(CommandReconnect, "alpha")
	wantActionStart(t, started, "reconnect:alpha")
	for range 10 {
		controller.Repair(CommandReconnect, "alpha")
		controller.Repair(CommandAuthorize, "beta")
	}
	select {
	case action := <-started:
		t.Fatalf("started duplicate action %q while reconnect was active", action)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	if got, want := wantModel(t, controller.Updates()), BuildMenu(status, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("post-reconnect model = %#v, want %#v", got, want)
	}

	controller.Repair(CommandAuthorize, "beta")
	wantActionStart(t, started, "authorize:beta")
	release <- struct{}{}
	if got, want := wantModel(t, controller.Updates()), BuildMenu(status, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("post-authorize model = %#v, want %#v", got, want)
	}
	select {
	case got := <-opened:
		if got != "https://login.example/authorize" {
			t.Fatalf("opened URL = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("authorize action did not open its approved URL")
	}
	if got := reconnects.Load(); got != 1 {
		t.Fatalf("reconnect calls = %d, want 1", got)
	}
	if got := authorizations.Load(); got != 1 {
		t.Fatalf("authorize calls = %d, want 1", got)
	}

	cancel()
	wantStopped(t, stopped)
}

func TestControllerActionFailure(t *testing.T) {
	status := actionableStatus()
	client := actionTestClient{
		status: func(context.Context) (Status, error) {
			return status, nil
		},
		reconnect: func(context.Context, string) error {
			return errors.New("provider secret detail")
		},
		authorize: func(context.Context, string) (string, error) {
			return "", errors.New("unexpected authorize")
		},
	}
	controller := newActionController(client, nil, make(chan time.Time))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)
	_ = wantModel(t, controller.Updates())

	controller.Repair(CommandReconnect, "alpha")
	got := wantModel(t, controller.Updates())
	if want := BuildMenu(status, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed-action model = %#v, want %#v", got, want)
	}
	for _, item := range got.Items {
		if strings.Contains(item.Label, "provider secret") {
			t.Fatalf("failed-action menu exposes raw error: %q", item.Label)
		}
	}

	cancel()
	wantStopped(t, stopped)
}

func TestControllerDoesNotReplay(t *testing.T) {
	status := actionableStatus()
	var statusCalls atomic.Int64
	var reconnects atomic.Int64
	client := actionTestClient{
		status: func(context.Context) (Status, error) {
			if statusCalls.Add(1) == 2 {
				return Status{}, errors.New("daemon restarted after unknown outcome")
			}
			return status, nil
		},
		reconnect: func(context.Context, string) error {
			reconnects.Add(1)
			return errors.New("outcome unknown")
		},
		authorize: func(context.Context, string) (string, error) {
			return "", errors.New("unexpected authorize")
		},
	}
	polls := make(chan time.Time)
	controller := newActionController(client, nil, polls)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)
	_ = wantModel(t, controller.Updates())

	controller.Repair(CommandReconnect, "alpha")
	if got, want := wantModel(t, controller.Updates()), BuildOfflineMenu(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown-outcome model = %#v, want %#v", got, want)
	}
	polls <- time.Now()
	_ = wantModel(t, controller.Updates())
	polls <- time.Now()
	_ = wantModel(t, controller.Updates())
	if got := reconnects.Load(); got != 1 {
		t.Fatalf("reconnect calls after recovery polls = %d, want no replay", got)
	}

	cancel()
	wantStopped(t, stopped)
}

func TestControllerActionShutdown(t *testing.T) {
	status := actionableStatus()
	actionStarted := make(chan struct{})
	releaseAction := make(chan struct{})
	actionFinished := make(chan struct{})
	var openerCalled atomic.Bool
	client := actionTestClient{
		status: func(context.Context) (Status, error) {
			return status, nil
		},
		reconnect: func(context.Context, string) error {
			return errors.New("unexpected reconnect")
		},
		authorize: func(context.Context, string) (string, error) {
			close(actionStarted)
			defer close(actionFinished)
			<-releaseAction
			return "https://login.example/authorize", nil
		},
	}
	controller := newActionController(client, func(context.Context, string) error {
		openerCalled.Store(true)
		return nil
	}, make(chan time.Time))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)
	_ = wantModel(t, controller.Updates())

	controller.Repair(CommandAuthorize, "beta")
	select {
	case <-actionStarted:
	case <-time.After(time.Second):
		t.Fatal("authorize action did not start")
	}
	cancel()
	stoppedEarly := false
	select {
	case <-stopped:
		stoppedEarly = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseAction)
	select {
	case <-actionFinished:
	case <-time.After(time.Second):
		t.Fatal("authorize action did not finish after release")
	}
	wantStopped(t, stopped)
	if stoppedEarly {
		t.Error("controller stopped before its active action finished")
	}
	if openerCalled.Load() {
		t.Fatal("controller opened an authorization URL after shutdown")
	}
}

func actionableStatus() Status {
	return Status{
		Backends: []BackendStatus{
			{Name: "alpha", State: "down", Label: "Not answering", RecommendedAction: ActionReconnect},
			{Name: "beta", State: "needs-auth", Label: "Needs authorizing", RecommendedAction: ActionAuthorize},
		},
	}
}

func wantActionStart(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("started action = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("action %q did not start", want)
	}
}
