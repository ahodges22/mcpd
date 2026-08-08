package web

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/secretstore/secretstoretest"
)

type retrySecretProvider struct {
	mu       sync.Mutex
	unlocked bool
}

func (p *retrySecretProvider) Get(_ context.Context, name string) (secretstore.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.unlocked {
		return secretstore.Result{}, &secretstore.Error{
			Operation: secretstore.OperationGet,
			Provider:  "test",
			Name:      name,
			Condition: secretstore.ConditionInteraction,
		}
	}
	return secretstore.Result{Value: "provider-only-secret", Present: true}, nil
}

func (*retrySecretProvider) Set(context.Context, string, string) error { return nil }

func (*retrySecretProvider) Delete(context.Context, string) error { return nil }

func (p *retrySecretProvider) Retry() {
	p.mu.Lock()
	p.unlocked = true
	p.mu.Unlock()
}

func TestSecretRoutesRequireConfiguredCoordinator(t *testing.T) {
	h := newHarness(t)
	res := h.get(t, "/api/secrets/-/status")
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/secrets/-/status = %d, want %d", res.status, http.StatusServiceUnavailable)
	}
}

func TestSecretAPIChallengeRequiresControlAuthenticator(t *testing.T) {
	h := newHarness(t)
	res := h.get(t, "/api/secrets/-/challenge?nonce=001122")
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/secrets/-/challenge = %d, want %d", res.status, http.StatusServiceUnavailable)
	}
}

func TestSecretAPIChallengeProvesDaemonIdentity(t *testing.T) {
	h := newHarness(t)
	authenticator, err := secretstore.EnsureControlAuthenticator(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	h.server.secretAuth = authenticator
	nonce, err := secretstore.NewControlNonce()
	if err != nil {
		t.Fatalf("NewControlNonce: %v", err)
	}
	res := h.get(t, "/api/secrets/-/challenge?nonce="+nonce)
	if res.status != http.StatusOK {
		t.Fatalf("GET /api/secrets/-/challenge = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	var out struct {
		Proof string `json:"proof"`
	}
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if !authenticator.Verify(nonce, out.Proof) {
		t.Fatal("challenge proof does not authenticate the daemon control key")
	}
}

func TestSecretAPINeverReturnsValues(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{
			Backends: map[string]config.Backend{
				"alpha": {HTTPURL: "https://alpha.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
			},
			Secrets: &config.Secrets{Provider: config.SecretProviderFile},
		},
		provider,
		func(string) (string, bool) { return "", false },
		secretstore.ResolutionTuning{},
		nil,
	)

	const value = "browser-only-secret"
	res := h.post(t, "/api/secrets/TOKEN", jsonBody(`{"value":"`+value+`"}`))
	if res.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/TOKEN = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if strings.Contains(res.body, value) {
		t.Fatalf("set response disclosed the stored value: %s", res.body)
	}
	status := h.get(t, "/api/secrets/-/status")
	if status.status != http.StatusOK {
		t.Fatalf("GET /api/secrets/-/status = %d (%s), want %d", status.status, status.body, http.StatusOK)
	}
	if !strings.Contains(status.body, `"name":"TOKEN"`) || !strings.Contains(status.body, `"source":"provider-present"`) {
		t.Fatalf("status response omitted allowlisted metadata: %s", status.body)
	}
	if strings.Contains(status.body, value) {
		t.Fatalf("status response disclosed the stored value: %s", status.body)
	}
	stored, err := provider.Get(t.Context(), "TOKEN")
	if err != nil || !stored.Present || stored.Value != value {
		t.Fatalf("stored result = %#v, error = %v", stored, err)
	}
}

func TestSecretAPIAcceptsMaximallyEscapedPortableValue(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{Backends: map[string]config.Backend{}, Secrets: &config.Secrets{Provider: config.SecretProviderFile}},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)
	value := strings.Repeat("<", secretstore.MaxValueBytes)
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	res := h.post(t, "/api/secrets/TOKEN", jsonBody(string(body)))
	if res.status != http.StatusOK {
		t.Fatalf("POST maximally escaped value = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if strings.Contains(res.body, value) {
		t.Fatal("set response disclosed the stored value")
	}
	stored, err := provider.Get(t.Context(), "TOKEN")
	if err != nil || !stored.Present || stored.Value != value {
		t.Fatalf("stored value length = %d, present = %v, error = %v", len(stored.Value), stored.Present, err)
	}
}

func TestSecretAPIOversizeBodyReportsPortableLimit(t *testing.T) {
	h := newHarness(t)
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{Backends: map[string]config.Backend{}, Secrets: &config.Secrets{Provider: config.SecretProviderFile}},
		secretstoretest.NewMemory(),
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)
	body, err := json.Marshal(map[string]string{"value": strings.Repeat("<", secretstore.MaxValueBytes+1024)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	res := h.post(t, "/api/secrets/TOKEN", jsonBody(string(body)))
	if res.status != http.StatusBadRequest {
		t.Fatalf("POST oversized body = %d, want %d", res.status, http.StatusBadRequest)
	}
	if !strings.Contains(res.body, `invalid_value`) || !strings.Contains(res.body, `limit 2048 bytes`) {
		t.Fatalf("oversized response omitted the portable value limit: %s", res.body)
	}
}

func TestSecretAPIWarnsWhenEnvironmentShadowsValue(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{Backends: map[string]config.Backend{}, Secrets: &config.Secrets{Provider: config.SecretProviderFile}},
		provider,
		func(name string) (string, bool) { return "daemon-environment-value", name == "TOKEN" },
		secretstore.ResolutionTuning{},
		nil,
	)

	res := h.post(t, "/api/secrets/TOKEN", jsonBody(`{"value":"stored-value"}`))
	if res.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/TOKEN = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if !strings.Contains(res.body, "environment") || !strings.Contains(res.body, "restart") {
		t.Fatalf("shadowed set response has no restart warning: %s", res.body)
	}
	if strings.Contains(res.body, "daemon-environment-value") || strings.Contains(res.body, "stored-value") {
		t.Fatalf("shadowed set response disclosed a secret value: %s", res.body)
	}
}

func TestSecretAPIUsesSharedOriginGuard(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{Backends: map[string]config.Backend{}, Secrets: &config.Secrets{Provider: config.SecretProviderFile}},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)

	res := h.post(t, "/api/secrets/TOKEN", jsonBody(`{"value":"must-not-be-stored"}`), origin("https://evil.example"))
	if !res.denied() {
		t.Fatalf("cross-origin secret set = %d (%s), want %d", res.status, res.body, http.StatusForbidden)
	}
	stored, err := provider.Get(t.Context(), "TOKEN")
	if err != nil || stored.Present {
		t.Fatalf("cross-origin request mutated provider: %#v, error = %v", stored, err)
	}
}

func TestSecretAPIRemoveReportsDependents(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	if err := provider.Set(t.Context(), "TOKEN", "remove-only-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{
			Backends: map[string]config.Backend{
				"alpha": {HTTPURL: "https://alpha.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
				"beta":  {HTTPURL: "https://beta.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
			},
			Secrets: &config.Secrets{Provider: config.SecretProviderFile},
		},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)

	res := h.post(t, "/api/secrets/TOKEN/remove")
	if res.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/TOKEN/remove = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if !strings.Contains(res.body, `"name":"alpha"`) || !strings.Contains(res.body, `"name":"beta"`) {
		t.Fatalf("remove response omitted dependents: %s", res.body)
	}
	if strings.Contains(res.body, "remove-only-secret") {
		t.Fatalf("remove response disclosed the stored value: %s", res.body)
	}
	stored, err := provider.Get(t.Context(), "TOKEN")
	if err != nil || stored.Present {
		t.Fatalf("removed provider result = %#v, error = %v", stored, err)
	}
}

func TestSecretAPIRefreshesPresenceStatus(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	if err := provider.Set(t.Context(), "TOKEN", "status-only-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{
			Backends: map[string]config.Backend{
				"alpha": {HTTPURL: "https://alpha.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
			},
			Secrets: &config.Secrets{Provider: config.SecretProviderFile},
		},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)
	first := h.get(t, "/api/secrets/-/status")
	if !strings.Contains(first.body, `"source":"provider-present"`) {
		t.Fatalf("initial status = %s", first.body)
	}
	if err := provider.Delete(t.Context(), "TOKEN"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	cached := h.get(t, "/api/secrets/-/status")
	if !strings.Contains(cached.body, `"source":"provider-present"`) {
		t.Fatalf("cached status was not retained: %s", cached.body)
	}

	refreshed := h.post(t, "/api/secrets/-/status/refresh")
	if refreshed.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/-/status/refresh = %d (%s), want %d", refreshed.status, refreshed.body, http.StatusOK)
	}
	if !strings.Contains(refreshed.body, `"source":"absent"`) || strings.Contains(refreshed.body, "status-only-secret") {
		t.Fatalf("refreshed status = %s", refreshed.body)
	}
}

func TestSecretAPIRetryClearsProviderCondition(t *testing.T) {
	h := newHarness(t)
	provider := &retrySecretProvider{}
	h.server.secrets = secretstore.NewResolutionCoordinator(
		&config.Config{
			Backends: map[string]config.Backend{
				"alpha": {HTTPURL: "https://alpha.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
			},
			Secrets: &config.Secrets{Provider: config.SecretProviderNative},
		},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		nil,
	)
	first := h.get(t, "/api/secrets/-/status")
	if !strings.Contains(first.body, `"condition":"interaction_required"`) {
		t.Fatalf("initial status = %s", first.body)
	}

	retry := h.post(t, "/api/secrets/-/retry")
	if retry.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/-/retry = %d (%s), want %d", retry.status, retry.body, http.StatusOK)
	}
	after := h.get(t, "/api/secrets/-/status")
	if !strings.Contains(after.body, `"source":"provider-present"`) || strings.Contains(after.body, "provider-only-secret") {
		t.Fatalf("status after retry = %s", after.body)
	}
}

func TestSecretAPICollectionRoutesDoNotShadowNames(t *testing.T) {
	for _, name := range []string{"status", "challenge", "retry"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			provider := secretstoretest.NewMemory()
			h.server.secrets = secretstore.NewResolutionCoordinator(
				&config.Config{Backends: map[string]config.Backend{}, Secrets: &config.Secrets{Provider: config.SecretProviderFile}},
				provider,
				nil,
				secretstore.ResolutionTuning{},
				nil,
			)
			res := h.post(t, "/api/secrets/"+name, jsonBody(`{"value":"stored-value"}`))
			if res.status != http.StatusOK {
				t.Fatalf("POST /api/secrets/%s = %d (%s), want %d", name, res.status, res.body, http.StatusOK)
			}
			stored, err := provider.Get(t.Context(), name)
			if err != nil || !stored.Present || stored.Value != "stored-value" {
				t.Fatalf("stored result = %#v, error = %v", stored, err)
			}
		})
	}
}

func TestSecretAPITargetedRefreshReconnectsExactConsumers(t *testing.T) {
	h := newHarness(t)
	provider := secretstoretest.NewMemory()
	if err := provider.Set(t.Context(), "TOKEN", "externally-updated-secret"); err != nil {
		t.Fatalf("Set TOKEN: %v", err)
	}
	if err := provider.Set(t.Context(), "OTHER", "unrelated-secret"); err != nil {
		t.Fatalf("Set OTHER: %v", err)
	}
	var mu sync.Mutex
	var reset, resolved []string
	coordinator := secretstore.NewResolutionCoordinator(
		&config.Config{
			Backends: map[string]config.Backend{
				"alpha": {HTTPURL: "https://alpha.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"}},
				"beta":  {HTTPURL: "https://beta.example/mcp", Headers: map[string]string{"Authorization": "Bearer ${OTHER}"}},
			},
			Secrets: &config.Secrets{Provider: config.SecretProviderFile},
		},
		provider,
		nil,
		secretstore.ResolutionTuning{},
		func(result secretstore.ResolvedConsumer) {
			mu.Lock()
			defer mu.Unlock()
			if result.Values["TOKEN"] != "externally-updated-secret" {
				t.Errorf("resolved values = %#v", result.Values)
			}
			resolved = append(resolved, result.Consumer.Name)
		},
	)
	coordinator.SetMutationHooks(secretstore.MutationHooks{Reset: func(consumer config.SecretConsumer) bool {
		mu.Lock()
		defer mu.Unlock()
		reset = append(reset, consumer.Name)
		return true
	}})
	h.server.secrets = coordinator

	res := h.post(t, "/api/secrets/TOKEN/refresh")
	if res.status != http.StatusOK {
		t.Fatalf("POST /api/secrets/TOKEN/refresh = %d (%s), want %d", res.status, res.body, http.StatusOK)
	}
	if !strings.Contains(res.body, `"name":"alpha"`) || strings.Contains(res.body, `"name":"beta"`) {
		t.Fatalf("targeted refresh dependents = %s", res.body)
	}
	if strings.Contains(res.body, "externally-updated-secret") || strings.Contains(res.body, "unrelated-secret") {
		t.Fatalf("targeted refresh response disclosed a value: %s", res.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(reset, ",") != "alpha" || strings.Join(resolved, ",") != "alpha" {
		t.Fatalf("reset = %v, resolved = %v", reset, resolved)
	}
}
