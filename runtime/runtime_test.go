package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newTestRuntime(t *testing.T, owner string) (*Runtime, Paths) {
	t.Helper()
	dir := t.TempDir()
	paths := Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	if err := os.WriteFile(paths.Config, []byte("{\"backends\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(context.Background(), Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7421/api/mcp/oauth/callback", Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return r, paths
}

func TestAdminStatusReportsSearchModelAndQueueState(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	if err := os.WriteFile(paths.Config, []byte(`{"backends":{},"embeddings":{"url":"http://127.0.0.1:1","model":"model-a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(context.Background(), Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7421/api/mcp/oauth/callback", Owner: "odo"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown(context.Background())
	response := httptest.NewRecorder()
	r.AdminHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var status StatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Search == nil || status.Search.Model != "model-a" || status.Search.CatalogTotal != 0 || status.Search.QueueState == "" {
		t.Fatalf("search status = %#v", status.Search)
	}
	unknown := httptest.NewRecorder()
	r.ProtocolHandler().ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown protocol path = %d", unknown.Code)
	}
}

func TestRuntimeOwnsStateAndHandlersRefuseAfterShutdown(t *testing.T) {
	r, paths := newTestRuntime(t, "odo")
	ownership, err := InspectOwnership(paths)
	if err != nil || !ownership.Held || ownership.Owner != "odo" || ownership.PID != os.Getpid() {
		t.Fatalf("ownership = %#v, %v", ownership, err)
	}
	_, err = New(context.Background(), Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7420/oauth/callback", Owner: "standalone"})
	var conflict *OwnershipError
	if !errors.As(err, &conflict) || conflict.Owner != "odo" {
		t.Fatalf("got %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	r.AdminHandler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/status", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", res.Code)
	}
	replacement, err := New(context.Background(), Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7420/oauth/callback", Owner: "standalone"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Shutdown(context.Background())
}
