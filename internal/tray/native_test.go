package tray

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const nativeTestTimeout = 300 * time.Millisecond

type fakeNativeDriver struct {
	mu            sync.Mutex
	applies       []MenuModel
	applyErrors   map[int]error
	blockApply    int
	applyEntered  chan struct{}
	applyEnterOne sync.Once
	applyRelease  chan struct{}
	ready         chan struct{}
	readyOnce     sync.Once
	stop          chan struct{}
	removeOnce    sync.Once
	removeCount   atomic.Int32
	runCount      atomic.Int32
	runStarted    chan struct{}
	runStartOnce  sync.Once
	runRelease    chan struct{}
	runErr        error
}

func newFakeNativeDriver() *fakeNativeDriver {
	return &fakeNativeDriver{
		applyErrors:  make(map[int]error),
		applyEntered: make(chan struct{}),
		ready:        make(chan struct{}),
		stop:         make(chan struct{}),
		runStarted:   make(chan struct{}),
	}
}

func (d *fakeNativeDriver) Apply(model MenuModel) error {
	d.mu.Lock()
	d.applies = append(d.applies, cloneMenuModel(model))
	call := len(d.applies)
	err := d.applyErrors[call]
	blocked := call == d.blockApply
	d.mu.Unlock()

	if blocked {
		d.applyEnterOne.Do(func() { close(d.applyEntered) })
		if d.applyRelease != nil {
			select {
			case <-d.applyRelease:
			case <-d.stop:
			}
		} else {
			<-d.stop
		}
	}
	return err
}

func (d *fakeNativeDriver) Ready() <-chan struct{} { return d.ready }

func (d *fakeNativeDriver) Run() error {
	d.runCount.Add(1)
	d.runStartOnce.Do(func() { close(d.runStarted) })
	if d.runRelease != nil {
		select {
		case <-d.runRelease:
		case <-d.stop:
		}
	} else {
		<-d.stop
	}
	return d.runErr
}

func (d *fakeNativeDriver) Remove() {
	d.removeCount.Add(1)
	d.removeOnce.Do(func() { close(d.stop) })
}

func (d *fakeNativeDriver) signalReady() {
	d.readyOnce.Do(func() { close(d.ready) })
}

func (d *fakeNativeDriver) snapshots() []MenuModel {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]MenuModel(nil), d.applies...)
}

func waitNativeCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(nativeTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func waitNativeRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(nativeTestTimeout):
		t.Fatal("native adapter Run did not return")
		return nil
	}
}

func nativeTestModel(label string) MenuModel {
	return MenuModel{
		Icon: IconAttention,
		Items: []MenuItem{{
			Label:    label,
			Children: []MenuItem{{Label: label + " child", Disabled: true}},
		}},
	}
}

func TestNativeAdapterLifecycle(t *testing.T) {
	t.Run("coalesces before ready and retries after apply error", func(t *testing.T) {
		driver := newFakeNativeDriver()
		driver.applyErrors[2] = errors.New("runtime apply")
		adapter := &NativeAdapter{driver: driver}
		ctx, cancel := context.WithCancel(context.Background())
		updates := make(chan MenuModel, 3)
		done := make(chan error, 1)
		go func() { done <- adapter.Run(ctx, updates) }()

		select {
		case <-driver.runStarted:
		case <-time.After(nativeTestTimeout):
			t.Fatal("native loop did not start")
		}
		updates <- nativeTestModel("stale")
		updates <- nativeTestModel("newest")
		time.Sleep(20 * time.Millisecond)
		if got := len(driver.snapshots()); got != 1 {
			t.Fatalf("Apply calls before ready = %d, want initial call only", got)
		}

		driver.signalReady()
		waitNativeCondition(t, func() bool { return len(driver.snapshots()) == 2 }, "coalesced snapshot was not applied")
		if got := driver.snapshots()[1].Items[0].Label; got != "newest" {
			t.Fatalf("first runtime snapshot = %q, want newest", got)
		}

		updates <- nativeTestModel("retry")
		waitNativeCondition(t, func() bool { return len(driver.snapshots()) == 3 }, "snapshot after apply failure was not retried")
		cancel()
		if err := waitNativeRun(t, done); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
		if got := driver.snapshots()[0]; got.Icon != IconOffline {
			t.Fatalf("initial icon = %q, want %q", got.Icon, IconOffline)
		}
	})

	t.Run("initial apply failure is recoverable", func(t *testing.T) {
		driver := newFakeNativeDriver()
		driver.applyErrors[1] = errors.New("initial apply")
		driver.signalReady()
		adapter := &NativeAdapter{driver: driver}
		ctx, cancel := context.WithCancel(context.Background())
		updates := make(chan MenuModel, 1)
		done := make(chan error, 1)
		go func() { done <- adapter.Run(ctx, updates) }()
		select {
		case <-driver.runStarted:
		case <-time.After(nativeTestTimeout):
			t.Fatal("native loop did not start after initial apply error")
		}
		updates <- nativeTestModel("recovered")
		waitNativeCondition(t, func() bool { return len(driver.snapshots()) == 2 }, "complete snapshot did not recover initial apply")
		cancel()
		if err := waitNativeRun(t, done); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
	})

	t.Run("already canceled skips apply and native run", func(t *testing.T) {
		driver := newFakeNativeDriver()
		adapter := &NativeAdapter{driver: driver}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := adapter.Run(ctx, make(chan MenuModel)); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if got := len(driver.snapshots()); got != 0 {
			t.Fatalf("Apply calls = %d, want 0", got)
		}
		if got := driver.runCount.Load(); got != 0 {
			t.Fatalf("native Run calls = %d, want 0", got)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
	})

	t.Run("cancellation during initial apply skips native run", func(t *testing.T) {
		driver := newFakeNativeDriver()
		driver.blockApply = 1
		driver.applyRelease = make(chan struct{})
		adapter := &NativeAdapter{driver: driver}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- adapter.Run(ctx, make(chan MenuModel)) }()
		select {
		case <-driver.applyEntered:
		case <-time.After(nativeTestTimeout):
			t.Fatal("initial Apply did not block")
		}
		cancel()
		close(driver.applyRelease)
		if err := waitNativeRun(t, done); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if got := driver.runCount.Load(); got != 0 {
			t.Fatalf("native Run calls = %d, want 0", got)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
	})

	t.Run("closed updates remove before join", func(t *testing.T) {
		driver := newFakeNativeDriver()
		driver.signalReady()
		adapter := &NativeAdapter{driver: driver}
		updates := make(chan MenuModel)
		close(updates)
		if err := adapter.Run(context.Background(), updates); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
	})

	t.Run("driver error releases blocked apply and is forwarded", func(t *testing.T) {
		wantErr := errors.New("native loop failed")
		driver := newFakeNativeDriver()
		driver.signalReady()
		driver.blockApply = 2
		driver.runRelease = make(chan struct{})
		driver.runErr = wantErr
		adapter := &NativeAdapter{driver: driver}
		updates := make(chan MenuModel, 1)
		done := make(chan error, 1)
		go func() { done <- adapter.Run(context.Background(), updates) }()
		updates <- nativeTestModel("blocked")
		select {
		case <-driver.applyEntered:
		case <-time.After(nativeTestTimeout):
			t.Fatal("runtime Apply did not block")
		}
		close(driver.runRelease)
		if err := waitNativeRun(t, done); !errors.Is(err, wantErr) {
			t.Fatalf("Run error = %v, want %v", err, wantErr)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
	})

	t.Run("cancellation releases blocked apply", func(t *testing.T) {
		driver := newFakeNativeDriver()
		driver.signalReady()
		driver.blockApply = 2
		adapter := &NativeAdapter{driver: driver}
		ctx, cancel := context.WithCancel(context.Background())
		updates := make(chan MenuModel, 32)
		done := make(chan error, 1)
		go func() { done <- adapter.Run(ctx, updates) }()
		updates <- nativeTestModel("blocked")
		select {
		case <-driver.applyEntered:
		case <-time.After(nativeTestTimeout):
			t.Fatal("runtime Apply did not block")
		}
		for i := 0; i < cap(updates); i++ {
			select {
			case updates <- nativeTestModel("latest"):
			default:
			}
		}
		cancel()
		if err := waitNativeRun(t, done); err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if got := driver.removeCount.Load(); got != 1 {
			t.Fatalf("Remove calls = %d, want 1", got)
		}
	})
}
