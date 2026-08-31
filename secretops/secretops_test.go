package secretops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ahodges22/mcpd/internal/stateowner"
)

func TestOfflineOperationRefusesRuntimeOwnership(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	config := `{"backends":{},"secrets":{"provider":"file"}}`
	if err := os.WriteFile(paths.Config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := stateowner.Acquire(paths.State, "odo")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, err = Status(context.Background(), paths)
	var conflict *stateowner.ConflictError
	if !errors.As(err, &conflict) || conflict.Owner != "odo" {
		t.Fatalf("got %v", err)
	}
}
