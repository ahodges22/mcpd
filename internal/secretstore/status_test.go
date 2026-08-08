package secretstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

func statusConfig() *config.Config {
	return &config.Config{
		Backends: map[string]config.Backend{
			"stdio": {
				Name:    "stdio",
				Command: "unused",
				Args:    []string{"${NOT_ALLOWLISTED}"},
				Env:     map[string]string{"TOKEN": "prefix-${STDIO_TOKEN}"},
				Headers: map[string]string{"Ignored": "${IGNORED_HEADER}"},
			},
			"http": {
				Name:    "http",
				HTTPURL: "https://${NOT_ALLOWLISTED}.example",
				Headers: map[string]string{"Authorization": "Bearer ${HTTP_TOKEN}"},
			},
		},
		Embeddings: config.Embeddings{URL: "https://example.test", APIKeyEnv: "EMBEDDINGS_TOKEN"},
		Secrets:    &config.Secrets{Provider: config.SecretProviderNative},
	}
}

func TestStatusUsesConfiguredReferencesOnly(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{Value: "value-" + name, Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		statusConfig(),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{},
		nil,
	)
	statuses := coordinator.Status(context.Background())
	want := []string{"EMBEDDINGS_TOKEN", "HTTP_TOKEN", "STDIO_TOKEN"}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %#v", statuses)
	}
	for i, name := range want {
		if statuses[i].Name != name || statuses[i].Source != EffectiveSourceProvider {
			t.Fatalf("status[%d] = %#v", i, statuses[i])
		}
		if provider.callCount(name) != 1 {
			t.Fatalf("provider calls for %s = %d", name, provider.callCount(name))
		}
	}
	for _, name := range []string{"NOT_ALLOWLISTED", "IGNORED_HEADER", "UNREFERENCED"} {
		if provider.callCount(name) != 0 {
			t.Fatalf("provider was queried for %s", name)
		}
	}
}

func TestEnvironmentStatusSkipsProvider(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{Value: "stored", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(name string) (string, bool) { return "", name == "TOKEN" },
		ResolutionTuning{},
		nil,
	)
	statuses := coordinator.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Source != EffectiveSourceEnvironment || statuses[0].Condition != "" {
		t.Fatalf("statuses = %#v", statuses)
	}
	if provider.callCount("TOKEN") != 0 {
		t.Fatal("environment-satisfied status probed the provider")
	}
}

func TestPresenceCacheStoresNoValue(t *testing.T) {
	const secret = "must-not-remain-in-status-cache"
	provider := &resolutionProvider{get: func(context.Context, string, int) (Result, error) {
		return Result{Value: secret, Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{},
		nil,
	)
	statuses := coordinator.Status(context.Background())
	if strings.Contains(fmt.Sprintf("%#v", coordinator.presence), secret) {
		t.Fatal("presence cache retained the provider value")
	}
	raw, err := json.Marshal(statuses)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("status disclosed provider value: %s", raw)
	}
}

func TestRepeatedStatusPollsUseOneProbe(t *testing.T) {
	provider := &resolutionProvider{get: func(context.Context, string, int) (Result, error) {
		return Result{Value: "secret", Present: true}, nil
	}}
	tuning := ResolutionTuning{PresenceTTL: time.Minute}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		tuning,
		nil,
	)
	for range 10 {
		statuses := coordinator.Status(context.Background())
		if len(statuses) != 1 || statuses[0].Source != EffectiveSourceProvider {
			t.Fatalf("statuses = %#v", statuses)
		}
	}
	if provider.callCount("TOKEN") != 1 {
		t.Fatalf("provider calls = %d, want one", provider.callCount("TOKEN"))
	}
}

func TestConcurrentStatusPollsUseOneProbe(t *testing.T) {
	provider := &resolutionProvider{get: func(context.Context, string, int) (Result, error) {
		time.Sleep(15 * time.Millisecond)
		return Result{Value: "secret", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{PresenceTTL: time.Minute},
		nil,
	)
	var wait sync.WaitGroup
	for range 10 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			coordinator.Status(context.Background())
		}()
	}
	wait.Wait()
	if provider.callCount("TOKEN") != 1 {
		t.Fatalf("provider calls = %d, want one", provider.callCount("TOKEN"))
	}
}

func TestStatusRefreshAndInvalidationProbeAgain(t *testing.T) {
	provider := &resolutionProvider{get: func(context.Context, string, int) (Result, error) {
		return Result{Value: "secret", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{PresenceTTL: time.Minute},
		nil,
	)
	coordinator.Status(context.Background())
	coordinator.RefreshStatus(context.Background())
	coordinator.InvalidatePresence("TOKEN")
	coordinator.Status(context.Background())
	if provider.callCount("TOKEN") != 3 {
		t.Fatalf("provider calls = %d, want initial, refresh, and invalidated probes", provider.callCount("TOKEN"))
	}
}

func TestStatusFailureUsesBackoffAndRedactsCause(t *testing.T) {
	const secret = "provider-cause-secret"
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionDenied, Cause: fmt.Errorf("denied %s", secret)}
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}, "beta": {"OTHER"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{FailureBackoff: time.Minute, FailureBackoffMax: time.Minute},
		nil,
	)
	for range 2 {
		statuses := coordinator.Status(context.Background())
		for _, status := range statuses {
			if status.Source != EffectiveSourceCondition || status.Condition != ConditionDenied {
				t.Fatalf("status = %#v", status)
			}
		}
		if strings.Contains(fmt.Sprintf("%#v", statuses), secret) {
			t.Fatal("status retained a provider error cause")
		}
	}
	if got := provider.callCount("TOKEN") + provider.callCount("OTHER"); got != 1 {
		t.Fatalf("provider calls = %d, want one health-establishing probe", got)
	}
}

func TestResolutionPopulatesPresenceCacheWithoutExtraProbe(t *testing.T) {
	provider := &resolutionProvider{get: func(context.Context, string, int) (Result, error) {
		return Result{Value: "secret", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{PresenceTTL: time.Minute},
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	statuses := coordinator.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Source != EffectiveSourceProvider {
		t.Fatalf("statuses = %#v", statuses)
	}
	if provider.callCount("TOKEN") != 1 {
		t.Fatalf("provider calls = %d, want startup lookup only", provider.callCount("TOKEN"))
	}
}

func TestResolutionFailurePopulatesConditionCache(t *testing.T) {
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInvalidValue}
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{FailureBackoff: time.Hour, FailureBackoffMax: time.Hour},
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.Start(ctx)
	statuses := coordinator.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Source != EffectiveSourceCondition || statuses[0].Condition != ConditionInvalidValue {
		t.Fatalf("statuses = %#v", statuses)
	}
	if provider.callCount("TOKEN") != 1 {
		t.Fatalf("provider calls = %d, want startup lookup only", provider.callCount("TOKEN"))
	}
}

func TestStatusSweepHasAggregateBudget(t *testing.T) {
	provider := &resolutionProvider{get: func(ctx context.Context, name string, _ int) (Result, error) {
		<-ctx.Done()
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionLockContended, Cause: ctx.Err()}
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{
			"alpha": {"A"},
			"beta":  {"B"},
			"gamma": {"C"},
		}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{CallTimeout: 100 * time.Millisecond, StatusBudget: 25 * time.Millisecond},
		nil,
	)
	started := time.Now()
	statuses := coordinator.Status(context.Background())
	if elapsed := time.Since(started); elapsed > 80*time.Millisecond {
		t.Fatalf("status sweep took %s", elapsed)
	}
	if len(statuses) != 3 {
		t.Fatalf("statuses = %#v", statuses)
	}
	if provider.callCount("A")+provider.callCount("B")+provider.callCount("C") != 1 {
		t.Fatalf("provider calls = A:%d B:%d C:%d", provider.callCount("A"), provider.callCount("B"), provider.callCount("C"))
	}
	if statuses[0].Condition != ConditionLockContended || statuses[1].Condition != ConditionTimedOut || statuses[2].Condition != ConditionTimedOut {
		t.Fatalf("statuses = %#v", statuses)
	}
}

func TestRetryInvalidatesPresenceConditions(t *testing.T) {
	available := false
	provider := &resolutionProvider{get: func(_ context.Context, name string, _ int) (Result, error) {
		if !available {
			return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInteraction}
		}
		return Result{Value: "secret", Present: true}, nil
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{FailureBackoff: time.Hour, FailureBackoffMax: time.Hour},
		nil,
	)
	first := coordinator.Status(context.Background())
	if len(first) != 1 || first[0].Condition != ConditionInteraction {
		t.Fatalf("first status = %#v", first)
	}
	available = true
	coordinator.Retry()
	second := coordinator.Status(context.Background())
	if len(second) != 1 || second[0].Source != EffectiveSourceProvider || second[0].Condition != "" {
		t.Fatalf("status after retry = %#v", second)
	}
	if provider.callCount("TOKEN") != 2 {
		t.Fatalf("provider calls = %d, want retry probe", provider.callCount("TOKEN"))
	}
}

func TestStatusBudgetDoesNotPoisonProviderHealth(t *testing.T) {
	provider := &resolutionProvider{get: func(ctx context.Context, name string, _ int) (Result, error) {
		<-ctx.Done()
		return Result{}, &Error{Operation: OperationGet, Provider: "test", Name: name, Condition: ConditionInteraction, Cause: ctx.Err()}
	}}
	coordinator := NewResolutionCoordinator(
		resolutionConfig(map[string][]string{"alpha": {"TOKEN"}}),
		provider,
		func(string) (string, bool) { return "", false },
		ResolutionTuning{CallTimeout: time.Second, StatusBudget: 15 * time.Millisecond},
		nil,
	)
	statuses := coordinator.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Condition != ConditionInteraction {
		t.Fatalf("statuses = %#v", statuses)
	}
	if condition, ok := coordinator.ProviderHealth(); ok {
		t.Fatalf("status budget latched provider health %q", condition)
	}
	if len(coordinator.presence) != 1 {
		t.Fatalf("status budget pacing entries = %#v", coordinator.presence)
	}
	again := coordinator.Status(context.Background())
	if len(again) != 1 || again[0].Condition != ConditionInteraction {
		t.Fatalf("cached status = %#v", again)
	}
	if provider.callCount("TOKEN") != 1 {
		t.Fatalf("provider calls = %d, want one paced probe", provider.callCount("TOKEN"))
	}
	if condition, ok := coordinator.ProviderHealth(); ok {
		t.Fatalf("paced status latched provider health %q", condition)
	}
}
