package clientconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetargetPreservesUnrelatedAndRefusesStaleBytes(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "{\n  \"mcpServers\": {\n    \"mcpd\": {\"url\": \"http://127.0.0.1:7420/mcp/search\"},\n    \"engram\": {\"url\": \"http://direct.example/mcp\"}\n  }\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "install"), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := "{\"client\":\"cursor\",\"path\":" + strconvQuote(path) + ",\"endpoint\":\"http://127.0.0.1:7420/mcp/search\"}\n"
	if err := os.WriteFile(filepath.Join(state, "install", "cursor.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanRetarget(home, state, "cursor", "127.0.0.1:7420", "127.0.0.1:7421")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(p.Notes, ""), "direct.example") {
		t.Fatal("plan exposed unrelated bytes")
	}
	if err := os.WriteFile(path, append([]byte(body), []byte(" ")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var conflict *ConflictError
	if err := Apply(context.Background(), p); !errors.As(err, &conflict) {
		t.Fatalf("got %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "127.0.0.1:7421") || !strings.Contains(string(got), "direct.example") {
		t.Fatalf("got %s", got)
	}
}

func TestRetargetedCodexInstallationCanBeReverted(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model = \"gpt-5\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	installPlan, err := PlanInstall(home, state, "codex", "127.0.0.1:7420")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), installPlan); err != nil {
		t.Fatal(err)
	}
	retargetPlan, err := PlanRetarget(home, state, "codex", "127.0.0.1:7420", "127.0.0.1:7421")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), retargetPlan); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".mcpd-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("backup count after install and retarget = %d", len(backups))
	}
	revertPlan, err := PlanRevert(home, state, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := Revert(context.Background(), revertPlan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("reverted bytes = %q", got)
	}
}

func TestRetargetReplanRepairsInterruptedReceiptWrite(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model = \"gpt-5\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	installPlan, err := PlanInstall(home, state, "codex", "127.0.0.1:7420")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), installPlan); err != nil {
		t.Fatal(err)
	}
	retargetPlan, err := PlanRetarget(home, state, "codex", "127.0.0.1:7420", "127.0.0.1:7421")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, retargetPlan.after, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryPlan, err := PlanRetarget(home, state, "codex", "127.0.0.1:7420", "127.0.0.1:7421")
	if err != nil {
		t.Fatal(err)
	}
	if recoveryPlan.kind != "retarget" || string(recoveryPlan.before) != string(recoveryPlan.after) {
		t.Fatalf("recovery plan = kind %q, file change %v", recoveryPlan.kind, string(recoveryPlan.before) != string(recoveryPlan.after))
	}
	if err := Apply(context.Background(), recoveryPlan); err != nil {
		t.Fatal(err)
	}
	revertPlan, err := PlanRevert(home, state, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := Revert(context.Background(), revertPlan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("reverted bytes = %q", got)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
