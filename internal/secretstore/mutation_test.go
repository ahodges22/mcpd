package secretstore

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/ahodges22/mcpd/internal/config"
)

type mutationProvider struct {
	mu       sync.Mutex
	values   map[string]string
	getErr   error
	getCalls int
}

func (p *mutationProvider) Get(_ context.Context, name string) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.getCalls++
	if p.getErr != nil {
		return Result{}, p.getErr
	}
	value, present := p.values[name]
	return Result{Value: value, Present: present}, nil
}

func (p *mutationProvider) Set(_ context.Context, name, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[name] = value
	return nil
}

func (p *mutationProvider) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.values, name)
	return nil
}

func TestSecretMutationReconnectsExactBackends(t *testing.T) {
	provider := &mutationProvider{values: map[string]string{"ALPHA_TOKEN": "old-alpha", "BETA_TOKEN": "old-beta"}}
	cfg := resolutionConfig(map[string][]string{
		"alpha": {"ALPHA_TOKEN"},
		"beta":  {"BETA_TOKEN"},
	})
	var reset, resolved []string
	coordinator := NewResolutionCoordinator(cfg, provider, func(string) (string, bool) { return "", false }, fastResolutionTuning(), func(group ResolvedConsumer) {
		resolved = append(resolved, group.Consumer.Name)
		if group.Values["ALPHA_TOKEN"] != "new-alpha" {
			t.Fatalf("resolved values = %#v", group.Values)
		}
	})
	coordinator.SetMutationHooks(MutationHooks{Reset: func(consumer config.SecretConsumer) bool {
		reset = append(reset, consumer.Name)
		return true
	}})

	dependents, err := coordinator.Set(t.Context(), "ALPHA_TOKEN", "new-alpha")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	wantDependents := []ConsumerIdentity{{Kind: config.ConsumerBackend, Name: "alpha"}}
	if !slices.Equal(dependents, wantDependents) {
		t.Fatalf("dependents = %#v, want %#v", dependents, wantDependents)
	}
	if !slices.Equal(reset, []string{"alpha"}) || !slices.Equal(resolved, []string{"alpha"}) {
		t.Fatalf("reset = %v, resolved = %v", reset, resolved)
	}
}

func TestReconnectStartsAfterMutationUnlock(t *testing.T) {
	provider := &mutationProvider{values: map[string]string{"TOKEN": "old"}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		nil,
	)
	checked := false
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool {
		if !provider.mu.TryLock() {
			t.Fatal("consumer reset started while the provider mutation lock was held")
		}
		provider.mu.Unlock()
		checked = true
		return true
	}})

	if _, err := coordinator.Set(t.Context(), "TOKEN", "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !checked {
		t.Fatal("consumer reset did not run")
	}
}

func TestMutationProviderFailureShortCircuitsLaterDependents(t *testing.T) {
	provider := &mutationProvider{
		values: map[string]string{"TOKEN": "old"},
		getErr: &Error{Operation: OperationGet, Provider: "test", Condition: ConditionUnavailable},
	}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}, "beta": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		nil,
	)
	coordinator.SetMutationHooks(MutationHooks{Reset: func(config.SecretConsumer) bool { return true }})
	if _, err := coordinator.Set(t.Context(), "TOKEN", "new"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	provider.mu.Lock()
	getCalls := provider.getCalls
	provider.mu.Unlock()
	if getCalls != 1 {
		t.Fatalf("provider gets = %d, want one health-establishing call", getCalls)
	}
	if pending := coordinator.Pending(); len(pending) != 2 {
		t.Fatalf("pending groups = %#v", pending)
	}
}

func TestDependencyIndexTracksBackendChanges(t *testing.T) {
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		&mutationProvider{values: map[string]string{}},
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		nil,
	)
	beta := config.Backend{Name: "beta", Command: "unused", Env: map[string]string{"TOKEN": "${OTHER_TOKEN}"}}
	coordinator.UpdateBackend("beta", &beta)
	if got := coordinator.Dependents("OTHER_TOKEN"); !slices.Equal(got, []ConsumerIdentity{{Kind: config.ConsumerBackend, Name: "beta"}}) {
		t.Fatalf("dependents after add = %#v", got)
	}
	coordinator.UpdateBackend("beta", nil)
	if got := coordinator.Dependents("OTHER_TOKEN"); len(got) != 0 {
		t.Fatalf("dependents after remove = %#v", got)
	}
}
