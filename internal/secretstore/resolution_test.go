package secretstore

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

type resolutionProvider struct {
	mu        sync.Mutex
	get       func(context.Context, string, int) (Result, error)
	calls     map[string]int
	active    int
	maxActive int
}

func (p *resolutionProvider) Get(ctx context.Context, name string) (Result, error) {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[name]++
	call := p.calls[name]
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()

	result, err := p.get(ctx, name, call)

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return result, err
}

func (p *resolutionProvider) Set(context.Context, string, string) error { return nil }
func (p *resolutionProvider) Delete(context.Context, string) error      { return nil }

func (p *resolutionProvider) callCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[name]
}

func resolutionConfig(groups map[string][]string) *config.Config {
	backends := make(map[string]config.Backend, len(groups))
	for name, refs := range groups {
		env := make(map[string]string, len(refs))
		for i, ref := range refs {
			env[string(rune('A'+i))] = "${" + ref + "}"
		}
		backends[name] = config.Backend{Name: name, Command: "unused", Env: env}
	}
	return &config.Config{
		Backends: backends,
		Secrets:  &config.Secrets{Provider: config.SecretProviderNative},
	}
}

func fastResolutionTuning() ResolutionTuning {
	return ResolutionTuning{
		StartupBudget:     35 * time.Millisecond,
		CallTimeout:       25 * time.Millisecond,
		BusyBackoffBase:   5 * time.Millisecond,
		BusyBackoffMax:    20 * time.Millisecond,
		FailureBackoff:    5 * time.Millisecond,
		FailureBackoffMax: 20 * time.Millisecond,
	}
}

func TestStartupResolutionIsGroupedAndBounded(t *testing.T) {
	provider := &resolutionProvider{get: func(ctx context.Context, name string, _ int) (Result, error) {
		if name == "B" {
			<-ctx.Done()
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionTimedOut, Cause: ctx.Err()}
		}
		return Result{Value: "value-" + name, Present: true}, nil
	}}
	resolved := make(chan ResolvedConsumer, 3)
	lookup := func(name string) (string, bool) {
		if name == "ENV_ONLY" {
			return "from environment", true
		}
		return "", false
	}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{
			"alpha": {"A", "B"},
			"beta":  {"C"},
			"gamma": {"ENV_ONLY"},
		}),
		provider,
		lookup,
		fastResolutionTuning(),
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.tuning.FailureBackoff = time.Hour
	coordinator.tuning.FailureBackoffMax = time.Hour
	started := time.Now()
	coordinator.Start(ctx)
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("startup resolution took %s", elapsed)
	}

	select {
	case group := <-resolved:
		if group.Consumer.Name != "gamma" || group.Values["ENV_ONLY"] != "from environment" {
			t.Fatalf("resolved group = %#v", group)
		}
	case <-time.After(time.Second):
		t.Fatal("environment-only group was blocked by provider resolution")
	}
	if provider.callCount("A") != 1 || provider.callCount("B") != 1 || provider.callCount("C") != 0 {
		t.Fatalf("provider calls = A:%d B:%d C:%d", provider.callCount("A"), provider.callCount("B"), provider.callCount("C"))
	}
	if pending := coordinator.Pending(); len(pending) != 2 {
		t.Fatalf("pending groups = %#v", pending)
	}
}

func TestPartialGroupIsDiscardedAndReResolved(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, call int) (Result, error) {
		if name == "B" && call == 1 {
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionUnavailable}
		}
		return Result{Value: name + "-value", Present: true}, nil
	}}
	resolved := make(chan ResolvedConsumer, 1)
	tuning := fastResolutionTuning()
	tuning.StartupBudget = time.Second
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A", "B"}}),
		provider,
		func(string) (string, bool) { return "", false },
		tuning,
		func(group ResolvedConsumer) {
			group.Values = maps.Clone(group.Values)
			resolved <- group
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	select {
	case group := <-resolved:
		if group.Values["A"] != "A-value" || group.Values["B"] != "B-value" {
			t.Fatalf("resolved values = %#v", group.Values)
		}
	case <-time.After(time.Second):
		t.Fatal("pending group did not recover")
	}
	if provider.callCount("A") != 2 || provider.callCount("B") != 2 {
		t.Fatalf("provider calls = A:%d B:%d; want full group twice", provider.callCount("A"), provider.callCount("B"))
	}
}

func TestResolvedValuesAreReleasedAfterConstruction(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{Value: "value-" + name, Present: true}, nil
	}}
	var temporary map[string]string
	called := false
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		func(group ResolvedConsumer) {
			called = true
			if group.Values["A"] != "value-A" {
				t.Fatalf("callback values = %#v", group.Values)
			}
			temporary = group.Values
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	if !called {
		t.Fatal("construction callback did not run")
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary values retained after construction: %#v", temporary)
	}
}

func TestPoisonedGroupDoesNotStarveLaterPendingGroups(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, call int) (Result, error) {
		if name == "A" {
			condition := ConditionInvalidValue
			if call == 1 {
				condition = ConditionUnavailable
			}
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: condition}
		}
		return Result{Value: "value-B", Present: true}, nil
	}}
	resolved := make(chan string, 1)
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}, "beta": {"B"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		func(group ResolvedConsumer) { resolved <- group.Consumer.Name },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	select {
	case name := <-resolved:
		if name != "beta" {
			t.Fatalf("resolved group = %q, want beta", name)
		}
	case <-time.After(time.Second):
		t.Fatal("poisoned group starved a later pending group")
	}
	if pending := coordinator.Pending(); len(pending) != 1 || pending[0].Consumer.Name != "alpha" {
		t.Fatalf("pending groups = %#v", pending)
	}
}

func TestStartupBudgetPreservesInteractionHealth(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		if name == "A" {
			time.Sleep(45 * time.Millisecond)
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInteraction}
		}
		return Result{Value: "value", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}, "beta": {"B"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	if condition, ok := coordinator.ProviderHealth(); !ok || condition != ConditionInteraction {
		t.Fatalf("provider health = %q, %v; want interaction required", condition, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if got := provider.callCount("A") + provider.callCount("B"); got != 1 {
		t.Fatalf("suspended interaction health made %d calls, want one", got)
	}
}

func TestWedgedProviderStartsOneHelperForManyGroups(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionWedged}
	}}
	tuning := fastResolutionTuning()
	tuning.FailureBackoff = time.Hour
	tuning.FailureBackoffMax = time.Hour
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}, "beta": {"B"}, "gamma": {"C"}}),
		provider,
		func(string) (string, bool) { return "", false },
		tuning,
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	if got := provider.callCount("A") + provider.callCount("B") + provider.callCount("C"); got != 1 {
		t.Fatalf("provider calls = %d, want one health-establishing helper", got)
	}
	if condition, ok := coordinator.ProviderHealth(); !ok || condition != ConditionWedged {
		t.Fatalf("provider health = %q, %v", condition, ok)
	}
	if pending := coordinator.Pending(); len(pending) != 3 {
		t.Fatalf("pending groups = %#v", pending)
	}
}

func TestPendingGroupsRecover(t *testing.T) {
	var mu sync.Mutex
	available := false
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if !available {
			available = true
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionUnavailable}
		}
		return Result{Value: "value-" + name, Present: true}, nil
	}}
	resolved := make(chan string, 2)
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}, "beta": {"B"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		func(group ResolvedConsumer) { resolved <- group.Consumer.Name },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	want := map[string]bool{"alpha": true, "beta": true}
	for range 2 {
		select {
		case name := <-resolved:
			delete(want, name)
		case <-time.After(time.Second):
			t.Fatalf("pending groups did not recover: %v", want)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing recovered groups: %v", want)
	}
	if provider.maxActive != 1 {
		t.Fatalf("provider max concurrency = %d, want serial work", provider.maxActive)
	}
	if _, ok := coordinator.ProviderHealth(); ok {
		t.Fatal("provider health remained latched after recovery")
	}
}

func TestResolveConsumerRemovesRecoveredPendingEntry(t *testing.T) {
	var mu sync.Mutex
	available := false
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if !available {
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInvalidValue}
		}
		return Result{Value: "resolved", Present: true}, nil
	}}
	cfg := resolutionConfig(map[string][]string{"alpha": {"A"}})
	tuning := fastResolutionTuning()
	tuning.FailureBackoff = time.Hour
	tuning.FailureBackoffMax = time.Hour
	coordinator := NewResolutionCoordinator(cfg, provider, func(string) (string, bool) { return "", false }, tuning, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	if pending := coordinator.Pending(); len(pending) != 1 {
		t.Fatalf("pending groups after startup = %#v", pending)
	}
	mu.Lock()
	available = true
	mu.Unlock()
	consumer := cfg.SecretConsumers()[0]
	values, err := coordinator.ResolveConsumer(t.Context(), consumer)
	if err != nil {
		t.Fatalf("ResolveConsumer: %v", err)
	}
	if values["A"] != "resolved" {
		t.Fatalf("resolved values = %#v", values)
	}
	clear(values)
	if pending := coordinator.Pending(); len(pending) != 0 {
		t.Fatalf("pending groups after synchronous recovery = %#v", pending)
	}
}

func TestBlockedResolveQueuesProviderConsumerAndAllowsEnvironmentConsumer(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionUnavailable}
	}}
	lookup := func(name string) (string, bool) {
		if name == "ENV" {
			return "environment", true
		}
		return "", false
	}
	tuning := fastResolutionTuning()
	tuning.FailureBackoff = time.Hour
	tuning.FailureBackoffMax = time.Hour
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}}),
		provider,
		lookup,
		tuning,
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	providerConsumer := config.SecretConsumer{Kind: config.ConsumerBackend, Name: "added", References: []string{"B"}}
	if _, err := coordinator.ResolveConsumer(t.Context(), providerConsumer); err == nil {
		t.Fatal("ResolveConsumer error = nil while provider health is blocked")
	}
	pending := coordinator.Pending()
	if len(pending) != 2 || pending[1].Consumer.Name != "added" {
		t.Fatalf("pending groups = %#v, want alpha and added", pending)
	}

	environmentConsumer := config.SecretConsumer{Kind: config.ConsumerBackend, Name: "environment", References: []string{"ENV"}}
	values, err := coordinator.ResolveConsumer(t.Context(), environmentConsumer)
	if err != nil {
		t.Fatalf("environment ResolveConsumer: %v", err)
	}
	if values["ENV"] != "environment" {
		t.Fatalf("environment values = %#v", values)
	}
	clear(values)
	if condition, ok := coordinator.ProviderHealth(); !ok || condition != ConditionUnavailable {
		t.Fatalf("provider health after environment-only resolution = %q, %v", condition, ok)
	}
}

func TestBusyContentionUsesPacingAndIsNotHealth(t *testing.T) {
	var mu sync.Mutex
	var attempts []time.Time
	provider := &resolutionProvider{get: func(_ context.Context, name string, call int) (Result, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()
		if call < 3 {
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionBusy}
		}
		return Result{Value: "value", Present: true}, nil
	}}
	resolved := make(chan struct{}, 1)
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		func(ResolvedConsumer) { resolved <- struct{}{} },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	select {
	case <-resolved:
	case <-time.After(time.Second):
		t.Fatal("busy group did not recover")
	}
	if _, ok := coordinator.ProviderHealth(); ok {
		t.Fatal("busy contention became provider health")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d", len(attempts))
	}
	if attempts[1].Sub(attempts[0]) < 4*time.Millisecond || attempts[2].Sub(attempts[1]) < 9*time.Millisecond {
		t.Fatalf("busy retries were not paced: %v", attempts)
	}
}

func TestProviderFailureUsesNegativeBackoff(t *testing.T) {
	var mu sync.Mutex
	var attempts []time.Time
	provider := &resolutionProvider{get: func(_ context.Context, name string, call int) (Result, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()
		if call < 3 {
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionUnavailable}
		}
		return Result{Value: "value", Present: true}, nil
	}}
	resolved := make(chan struct{}, 1)
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		func(ResolvedConsumer) { resolved <- struct{}{} },
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)

	select {
	case <-resolved:
	case <-time.After(time.Second):
		t.Fatal("provider failure did not recover")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d", len(attempts))
	}
	if attempts[1].Sub(attempts[0]) < 4*time.Millisecond || attempts[2].Sub(attempts[1]) < 9*time.Millisecond {
		t.Fatalf("provider retries did not back off: %v", attempts)
	}
}

func TestPerGroupFailureUsesNegativeBackoff(t *testing.T) {
	attempts := make(chan time.Time, 4)
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		attempts <- time.Now()
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInvalidValue}
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"A"}}),
		provider,
		func(string) (string, bool) { return "", false },
		fastResolutionTuning(),
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	times := make([]time.Time, 0, 3)
	for len(times) < 3 {
		select {
		case attempt := <-attempts:
			times = append(times, attempt)
		case <-time.After(time.Second):
			t.Fatalf("received %d attempts, want three", len(times))
		}
	}
	if times[1].Sub(times[0]) < 4*time.Millisecond || times[2].Sub(times[1]) < 9*time.Millisecond {
		t.Fatalf("per-group retries did not back off: %v", times)
	}
}
