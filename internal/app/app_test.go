package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ahodges22/mcpd/internal/stateowner"
)

func TestFailedConstructionReleasesStateOwnership(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	if err := os.WriteFile(paths.Config, []byte("{\"not_backends\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7421/api/mcp/oauth/callback", Owner: "odo"}); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	lease, err := stateowner.Acquire(paths.State, "replacement")
	if err != nil {
		t.Fatalf("failed constructor retained ownership: %v", err)
	}
	defer lease.Close()
}
