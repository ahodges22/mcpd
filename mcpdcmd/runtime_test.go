package mcpdcmd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	mcpdruntime "github.com/ahodges22/mcpd/runtime"
	publicowner "github.com/ahodges22/mcpd/stateowner"
)

func TestListenerBindFailureReleasesRuntimeOwnership(t *testing.T) {
	dir := t.TempDir()
	paths := mcpdruntime.Paths{Config: filepath.Join(dir, "config.json"), State: filepath.Join(dir, "state")}
	if err := os.WriteFile(paths.Config, []byte("{\"backends\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := mcpdruntime.New(context.Background(), mcpdruntime.Options{Paths: paths, OAuthCallbackURL: "http://127.0.0.1:7420/oauth/callback", Owner: "standalone mcpd"})
	if err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err := serveRuntime(context.Background(), occupied.Addr().String(), rt, nil); err == nil {
		t.Fatal("bind unexpectedly succeeded")
	}
	lease, err := publicowner.Acquire(paths.State, "replacement")
	if err != nil {
		t.Fatalf("bind failure retained ownership: %v", err)
	}
	defer lease.Close()
}
