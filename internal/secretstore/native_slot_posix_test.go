//go:build darwin || linux

package secretstore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeSlotSerializesProcesses(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	slot, err := NewNativeSlot(state)
	if err != nil {
		t.Fatalf("NewNativeSlot: %v", err)
	}
	lockPath := filepath.Join(slot.dir, nativeHelperLockName)
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat lock: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeSlotProcessHelper$")
	cmd.Env = append(os.Environ(), "MCPD_NATIVE_SLOT_HELPER=1", "MCPD_NATIVE_SLOT_STATE="+state)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start helper: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		_ = stdin.Close()
		_ = cmd.Wait()
		t.Fatalf("helper readiness = %q, %v", scanner.Text(), scanner.Err())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	started := time.Now()
	_, err = slot.Acquire(ctx, OperationGet, "TOKEN")
	cancel()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Acquire returned after %s, want bounded return", elapsed)
	}
	assertCondition(t, err, ConditionLockContended)
	if IsProviderHealthError(err) {
		t.Fatal("cross-process contention was classified as provider health")
	}

	if err := stdin.Close(); err != nil {
		t.Fatalf("close helper stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
	stopped = true
	lease, err := slot.Acquire(context.Background(), OperationGet, "TOKEN")
	if err != nil {
		t.Fatalf("Acquire after helper exit: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat lock after acquisition: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("native helper lock was replaced")
	}
	if got := after.Mode().Perm(); got != 0o600 {
		t.Fatalf("native helper lock mode = %o, want 600", got)
	}
}

func TestNativeBusyIsNotHealth(t *testing.T) {
	state := filepath.Join(stateSandbox(t), "state")
	first, err := NewNativeSlot(state)
	if err != nil {
		t.Fatalf("NewNativeSlot first: %v", err)
	}
	second, err := NewNativeSlot(state)
	if err != nil {
		t.Fatalf("NewNativeSlot second: %v", err)
	}
	lease, err := first.Acquire(context.Background(), OperationGet, "TOKEN")
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	defer lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = second.Acquire(ctx, OperationGet, "TOKEN")
	assertCondition(t, err, ConditionBusy)
	if IsProviderHealthError(err) {
		t.Fatal("in-process busy was classified as provider health")
	}
}

func TestNativeSlotRejectsExpiredContext(t *testing.T) {
	slot, err := NewNativeSlot(filepath.Join(stateSandbox(t), "state"))
	if err != nil {
		t.Fatalf("NewNativeSlot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		lease, err := slot.Acquire(ctx, OperationGet, "TOKEN")
		if lease != nil {
			_ = lease.Release()
			t.Fatal("Acquire succeeded with an expired context")
		}
		assertCondition(t, err, ConditionBusy)
	}
}

func TestNativeSlotProcessHelper(t *testing.T) {
	if os.Getenv("MCPD_NATIVE_SLOT_HELPER") != "1" {
		return
	}
	slot, err := NewNativeSlot(os.Getenv("MCPD_NATIVE_SLOT_STATE"))
	if err != nil {
		fmt.Fprintln(os.Stdout, "ERROR", err)
		os.Exit(2)
	}
	lease, err := slot.Acquire(context.Background(), OperationHealth, "")
	if err != nil {
		fmt.Fprintln(os.Stdout, "ERROR", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lease.Release(); err != nil {
		os.Exit(2)
	}
}
