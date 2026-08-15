package tray

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type statusFunc func(context.Context) (Status, error)

func (f statusFunc) Status(ctx context.Context) (Status, error) { return f(ctx) }

func TestControllerPolling(t *testing.T) {
	if controllerPollInterval != 5*time.Second {
		t.Fatalf("controller poll interval = %s, want 5s", controllerPollInterval)
	}

	polls := make(chan time.Time)
	blocked := make(chan struct{})
	blockedStarted := make(chan struct{})
	calls := make(chan int, 8)
	var callCount atomic.Int64
	client := statusFunc(func(ctx context.Context) (Status, error) {
		call := int(callCount.Add(1))
		calls <- call
		if call == 4 {
			close(blockedStarted)
			select {
			case <-blocked:
			case <-ctx.Done():
				return Status{}, ctx.Err()
			}
		}
		return pollingStatus(call), nil
	})
	controller := newController(client, polls)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)

	wantCall(t, calls, 1)
	polls <- time.Now()
	wantCall(t, calls, 2)
	polls <- time.Now()
	wantCall(t, calls, 3)
	polls <- time.Now()
	wantCall(t, calls, 4)
	select {
	case <-blockedStarted:
	case <-time.After(time.Second):
		t.Fatal("fourth status poll did not block")
	}

	// The adapter has not consumed an update yet. It must receive the newest
	// complete snapshot rather than a queue of stale intermediate models. The
	// blocked fourth request proves the third model has finished publishing.
	want := BuildMenu(pollingStatus(3), false)
	if got := wantModel(t, controller.Updates()); !reflect.DeepEqual(got, want) {
		t.Fatalf("latest model = %#v, want %#v", got, want)
	}

	callbackReturned := make(chan struct{})
	go func() {
		controller.Retry()
		close(callbackReturned)
	}()
	select {
	case <-callbackReturned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Retry blocked a native callback while a poll was active")
	}
	close(blocked)
	wantCall(t, calls, 5)

	cancel()
	wantStopped(t, stopped)
}

func TestControllerOfflineRecovery(t *testing.T) {
	polls := make(chan time.Time)
	var calls atomic.Int64
	recovered := Status{
		Serving:  1,
		Backends: []BackendStatus{{Name: "alpha", State: "up", Label: "Serving"}},
	}
	client := statusFunc(func(context.Context) (Status, error) {
		if calls.Add(1) == 1 {
			return Status{}, errors.New("daemon unavailable")
		}
		return recovered, nil
	})
	controller := newController(client, polls)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)

	if got, want := wantModel(t, controller.Updates()), BuildOfflineMenu(); !reflect.DeepEqual(got, want) {
		t.Fatalf("offline model = %#v, want %#v", got, want)
	}
	polls <- time.Now()
	if got, want := wantModel(t, controller.Updates()), BuildMenu(recovered, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered model = %#v, want %#v", got, want)
	}

	cancel()
	wantStopped(t, stopped)
}

func TestControllerShutdown(t *testing.T) {
	started := make(chan struct{})
	client := statusFunc(func(ctx context.Context) (Status, error) {
		close(started)
		<-ctx.Done()
		return Status{}, ctx.Err()
	})
	controller := newController(client, make(chan time.Time))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := runController(controller, ctx)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup poll did not begin")
	}
	cancel()
	wantStopped(t, stopped)
	if _, ok := <-controller.Updates(); ok {
		t.Fatal("controller published an offline model after shutdown cancellation")
	}
}

func pollingStatus(call int) Status {
	backends := []BackendStatus{
		{Name: "alpha", State: "up", Label: "Serving"},
		{Name: "beta", State: "up", Label: "Serving"},
		{Name: "gamma", State: "up", Label: "Serving"},
	}
	return Status{Serving: call, Backends: backends}
}

func runController(controller *Controller, ctx context.Context) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		controller.Run(ctx)
		close(stopped)
	}()
	return stopped
}

func wantCall(t *testing.T, calls <-chan int, want int) {
	t.Helper()
	select {
	case got := <-calls:
		if got != want {
			t.Fatalf("status call = %d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("status call %d did not start", want)
	}
}

func wantModel(t *testing.T, updates <-chan MenuModel) MenuModel {
	t.Helper()
	select {
	case model, ok := <-updates:
		if !ok {
			t.Fatal("controller updates closed before the expected model")
		}
		return model
	case <-time.After(time.Second):
		t.Fatal("controller did not publish a model")
		return MenuModel{}
	}
}

func wantStopped(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("controller did not stop after cancellation")
	}
}
