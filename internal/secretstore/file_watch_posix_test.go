//go:build darwin || linux

package secretstore

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

func TestExternalAtomicReplacementReloadsFinalSnapshot(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Set(t.Context(), "TOKEN", "old"); err != nil {
		t.Fatalf("Set TOKEN: %v", err)
	}
	if err := store.Set(t.Context(), "OTHER", "same"); err != nil {
		t.Fatalf("Set OTHER: %v", err)
	}
	if err := store.Set(t.Context(), "SECOND", "old-second"); err != nil {
		t.Fatalf("Set SECOND: %v", err)
	}

	resolved := make(chan ResolvedConsumer, 4)
	var resetMu sync.Mutex
	var reset []string
	tuning := fastResolutionTuning()
	tuning.FileWatchDebounce = 50 * time.Millisecond
	tuning.FileWatchPollInterval = time.Hour
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN", "SECOND"}, "beta": {"OTHER"}}),
		store,
		func(string) (string, bool) { return "", false },
		tuning,
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	coordinator.SetMutationHooks(MutationHooks{Reset: func(consumer config.SecretConsumer) bool {
		resetMu.Lock()
		reset = append(reset, consumer.Name)
		resetMu.Unlock()
		return true
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	for range 2 {
		select {
		case <-resolved:
		case <-time.After(time.Second):
			t.Fatal("startup resolution did not complete")
		}
	}

	externalAtomicReplace(t, state, map[string]string{"TOKEN": "intermediate", "SECOND": "intermediate-second", "OTHER": "same"})
	externalAtomicReplace(t, state, map[string]string{"TOKEN": "final", "SECOND": "final-second", "OTHER": "same"})

	select {
	case got := <-resolved:
		if got.Consumer.Name != "alpha" || got.Values["TOKEN"] != "final" || got.Values["SECOND"] != "final-second" {
			t.Fatalf("external refresh = %#v, want alpha with final value", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("external replacement did not refresh its consumer")
	}
	time.Sleep(3 * tuning.FileWatchDebounce)
	resetMu.Lock()
	defer resetMu.Unlock()
	if !slices.Equal(reset, []string{"alpha"}) {
		t.Fatalf("reset consumers = %v, want exact alpha refresh", reset)
	}
}

func TestMetadataFallbackDetectsChange(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Set(t.Context(), "TOKEN", "old"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	store.disableWatchEvents = true

	resolved := make(chan ResolvedConsumer, 2)
	tuning := fastResolutionTuning()
	tuning.FileWatchDebounce = 5 * time.Millisecond
	tuning.FileWatchPollInterval = 20 * time.Millisecond
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		store,
		func(string) (string, bool) { return "", false },
		tuning,
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	<-resolved

	externalAtomicReplace(t, state, map[string]string{"TOKEN": "from-poll"})
	select {
	case got := <-resolved:
		if got.Values["TOKEN"] != "from-poll" {
			t.Fatalf("metadata refresh values = %#v", got.Values)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata fallback did not detect replacement")
	}
}

func TestDaemonWriteTriggersOneReconnect(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	tuning := fastResolutionTuning()
	tuning.FileWatchDebounce = 30 * time.Millisecond
	tuning.FileWatchPollInterval = 50 * time.Millisecond
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		store,
		func(string) (string, bool) { return "", false },
		tuning,
		nil,
	)
	var mu sync.Mutex
	resets := 0
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool {
		mu.Lock()
		resets++
		mu.Unlock()
		return true
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	if _, err := coordinator.Set(t.Context(), "TOKEN", "daemon-write"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(5 * tuning.FileWatchPollInterval)
	mu.Lock()
	defer mu.Unlock()
	if resets != 1 {
		t.Fatalf("reconnects after daemon write = %d, want 1", resets)
	}
}

func TestUnrelatedStateWritesDoNotDelaySecretReload(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Set(t.Context(), "TOKEN", "old"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	resolved := make(chan ResolvedConsumer, 2)
	tuning := fastResolutionTuning()
	tuning.FileWatchDebounce = 40 * time.Millisecond
	tuning.FileWatchPollInterval = time.Hour
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		store,
		func(string) (string, bool) { return "", false },
		tuning,
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	<-resolved

	externalAtomicReplace(t, state, map[string]string{"TOKEN": "new"})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(state, "catalog.json"), []byte(fmt.Sprintf("%d", i)), 0o600)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	select {
	case got := <-resolved:
		if got.Values["TOKEN"] != "new" {
			t.Fatalf("refresh values = %#v", got.Values)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("unrelated state writes delayed the secret reload")
	}
}

func TestContinuousSecretReplacementsCannotStarveReload(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	store, err := NewFileStore(state)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.Set(t.Context(), "TOKEN", "old"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	resolved := make(chan ResolvedConsumer, 8)
	tuning := fastResolutionTuning()
	tuning.FileWatchDebounce = 20 * time.Millisecond
	tuning.FileWatchPollInterval = time.Hour
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		store,
		func(string) (string, bool) { return "", false },
		tuning,
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	<-resolved

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := atomicReplace(state, map[string]string{"TOKEN": fmt.Sprintf("value-%d", i)}); err != nil {
				done <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Errorf("continuous replacement: %v", err)
		}
	}()

	select {
	case <-resolved:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("continuous replacements starved the bounded reload")
	}
}

func externalAtomicReplace(t *testing.T, state string, values map[string]string) {
	t.Helper()
	if err := atomicReplace(state, values); err != nil {
		t.Fatalf("atomic replace: %v", err)
	}
}

func atomicReplace(state string, values map[string]string) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(state, ".external-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(state, fileStoreDataName)); err != nil {
		return err
	}
	return nil
}
