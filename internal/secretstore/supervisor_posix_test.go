//go:build darwin || linux

package secretstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPOSIXHelperIsSessionLeader(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(2 * time.Second)}, "")
	if err != nil {
		var providerErr *Error
		if errors.As(err, &providerErr) {
			t.Fatalf("Run: %v (cause %v)", err, providerErr.Cause)
		}
		t.Fatalf("Run: %v", err)
	}
	pid, err := strconv.Atoi(result.Value)
	if err != nil {
		t.Fatalf("helper pid %q: %v", result.Value, err)
	}
	if result.Present != true || pid <= 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestPreMarkerFailureSendsNoRequest(t *testing.T) {
	root := stateSandbox(t)
	state := filepath.Join(root, "state")
	received := filepath.Join(root, "request-received")
	supervisor, err := NewPOSIXSupervisor(state, os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_REQUEST_RECEIVED="+received)
	supervisor.writeMarker = func(helperMarker) error { return errors.New("injected marker failure") }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(2 * time.Second)}, "")
	if err == nil {
		t.Fatal("Run succeeded despite marker failure")
	}
	if _, statErr := os.Stat(received); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("helper received request before marker durability: %v", statErr)
	}
}

func TestPOSIXSupervisorProcessHelper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	request, _, err := ReadHelperRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if path := os.Getenv("MCPD_REQUEST_RECEIVED"); path != "" {
		_ = os.WriteFile(path, []byte("received"), 0o600)
	}
	pgid, err := unix.Getpgid(0)
	if err != nil || pgid != os.Getpid() {
		_ = WriteHelperResponse(os.Stdout, HelperResponseFromError(fmt.Errorf("pgid %d pid %d: %v", pgid, os.Getpid(), err)))
		os.Exit(0)
	}
	sid, err := unix.Getsid(0)
	if err != nil || sid != os.Getpid() {
		_ = WriteHelperResponse(os.Stdout, HelperResponseFromError(fmt.Errorf("sid %d pid %d: %v", sid, os.Getpid(), err)))
		os.Exit(0)
	}
	if os.Getenv("MCPD_BLOCK_HELPER") == "1" {
		if path := os.Getenv("MCPD_TERM_RECEIVED"); path != "" {
			term := make(chan os.Signal, 1)
			signal.Notify(term, syscall.SIGTERM)
			go func() {
				<-term
				_ = os.WriteFile(path, []byte("received"), 0o600)
			}()
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("MCPD_RESPONSE_THEN_BLOCK") == "1" {
		_ = WriteHelperResponse(os.Stdout, HelperResponse{Result: Result{Value: "blocked", Present: true}})
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("MCPD_RETURN_HELPER_ID") == "1" {
		_ = WriteHelperResponse(os.Stdout, HelperResponse{Result: Result{Value: os.Getenv(nativeHelperIDEnv), Present: true}})
		os.Exit(0)
	}
	if os.Getenv("MCPD_EXIT_WITHOUT_RESPONSE") == "1" {
		os.Exit(2)
	}
	if path := os.Getenv("MCPD_DESCENDANT_PIDS"); path != "" {
		child := exec.Command("sleep", "30")
		child.Env = os.Environ()
		if os.Getenv("MCPD_UNTAGGED_DESCENDANT") == "1" {
			child.Env = []string{"PATH=" + os.Getenv("PATH")}
		}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0o600)
		if os.Getenv("MCPD_EXIT_AFTER_DESCENDANT") == "1" {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if request.Operation == OperationGet {
		_ = WriteHelperResponse(os.Stdout, HelperResponse{Result: Result{Value: strconv.Itoa(os.Getpid()), Present: true}})
		os.Exit(0)
	}
	_ = WriteHelperResponse(os.Stdout, HelperResponse{})
	os.Exit(0)
}

func TestRequestDeadlineBoundsHelper(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_BLOCK_HELPER=1")
	supervisor.terminationWait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: start.Add(100 * time.Millisecond)}, "")
	assertCondition(t, err, ConditionTimedOut)
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("request deadline did not bound helper: %s", elapsed)
	}
}

func TestTimeoutRequestsTerminationBeforeKill(t *testing.T) {
	root := stateSandbox(t)
	received := filepath.Join(root, "term-received")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_BLOCK_HELPER=1", "MCPD_TERM_RECEIVED="+received)
	supervisor.terminationWait = 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(150 * time.Millisecond)}, "")
	assertCondition(t, err, ConditionTimedOut)
	if _, err := os.Stat(received); err != nil {
		t.Fatalf("helper did not receive termination request: %v", err)
	}
}

func TestResponseDoesNotPermitUnboundedExit(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_RESPONSE_THEN_BLOCK=1")
	supervisor.terminationWait = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	start := time.Now()
	go func() {
		_, runErr := supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: start.Add(100 * time.Millisecond)}, "")
		result <- runErr
	}()
	select {
	case runErr := <-result:
		assertCondition(t, runErr, ConditionTimedOut)
		if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
			t.Fatalf("response permitted unbounded helper exit: %s", elapsed)
		}
	case <-time.After(400 * time.Millisecond):
		marker, readErr := supervisor.readMarker()
		if readErr == nil {
			_, _ = supervisor.terminateMarkerGroup(marker)
		}
		<-result
		t.Fatal("response permitted unbounded helper exit")
	}
}

func TestGeneratedHelperIDReplacesParentValue(t *testing.T) {
	t.Setenv(nativeHelperIDEnv, "parent-value")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_RETURN_HELPER_ID=1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(2 * time.Second)}, "")
	if err != nil {
		var providerErr *Error
		if errors.As(err, &providerErr) {
			t.Fatalf("Run: %v (cause %v)", err, providerErr.Cause)
		}
		t.Fatalf("Run: %v", err)
	}
	if result.Value == "parent-value" || len(result.Value) != 32 {
		t.Fatalf("helper instance id = %q", result.Value)
	}
}

func TestMutationCrashIsIndeterminate(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_EXIT_WITHOUT_RESPONSE=1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationSet, Name: "TOKEN", Deadline: time.Now().Add(500 * time.Millisecond)}, "value")
	assertCondition(t, err, ConditionIndeterminate)
}

func TestPrivateSessionHandlesUntaggedMember(t *testing.T) {
	root := stateSandbox(t)
	pidsPath := filepath.Join(root, "pids")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_DESCENDANT_PIDS="+pidsPath, "MCPD_UNTAGGED_DESCENDANT=1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, runErr := supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(750 * time.Millisecond)}, "")
		result <- runErr
	}()
	var data []byte
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(data) == 0 && time.Now().Before(deadline) {
		data, _ = os.ReadFile(pidsPath)
		if len(data) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(data) == 0 {
		t.Fatal("helper did not report descendant pid")
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("pids = %q", data)
	}
	helperPID, _ := strconv.Atoi(fields[0])
	descendant, _ := strconv.Atoi(fields[1])
	var descendantIdentity posixProcessIdentity
	found := false
	for !found && time.Now().Before(deadline) {
		members, inspectErr := inspectPOSIXGroup(helperPID)
		if inspectErr != nil {
			t.Fatalf("inspect process group: %v", inspectErr)
		}
		for _, member := range members {
			if member.PID == descendant {
				descendantIdentity = member
				found = true
				break
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("descendant %d was absent from process group %d", descendant, helperPID)
	}
	err = <-result
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	if descendantIdentity.EnvironmentVisible {
		assertCondition(t, err, ConditionWedged)
		if err := syscall.Kill(descendant, 0); err != nil {
			t.Fatalf("unproven group member was signaled: %v", err)
		}
		return
	}
	assertCondition(t, err, ConditionTimedOut)
	if err := syscall.Kill(descendant, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("private-session member survived timeout: %v", err)
	}
}

func TestPOSIXSupervisorTimeoutTerminatesTree(t *testing.T) {
	root := stateSandbox(t)
	pidsPath := filepath.Join(root, "pids")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_DESCENDANT_PIDS="+pidsPath)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(250 * time.Millisecond)}, "")
	assertCondition(t, err, ConditionTimedOut)
	data, readErr := os.ReadFile(pidsPath)
	if readErr != nil {
		t.Fatalf("ReadFile pids: %v", readErr)
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pid %q: %v", field, err)
		}
		_, inspectErr := inspectPOSIXProcess(pid)
		if !processGone(inspectErr) {
			t.Fatalf("process %d survived timeout: %v", pid, inspectErr)
		}
	}
}

func TestHelperExitDoesNotLeakTree(t *testing.T) {
	root := stateSandbox(t)
	pidsPath := filepath.Join(root, "pids")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv, "MCPD_DESCENDANT_PIDS="+pidsPath, "MCPD_EXIT_AFTER_DESCENDANT=1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(2 * time.Second)}, "")
	assertCondition(t, err, ConditionUnexpected)
	data, readErr := os.ReadFile(pidsPath)
	if readErr != nil {
		t.Fatalf("ReadFile pids: %v", readErr)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("pids = %q", data)
	}
	descendant, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("descendant pid %q: %v", fields[1], err)
	}
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	if _, inspectErr := inspectPOSIXProcess(descendant); !processGone(inspectErr) {
		t.Fatalf("descendant survived helper exit: %v", inspectErr)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker remains after helper exit cleanup: %v", err)
	}
}

func TestRecoveryNeverSignalsUnrelatedProcess(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start unrelated process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect unrelated process: %v", err)
	}
	marker := markerForIdentity(identity, "not-the-process-instance")
	marker.Phase = helperPhaseWedged
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	if err := supervisor.Revalidate(context.Background()); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was signaled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale marker remains: %v", err)
	}
}

func TestRecoveryRemovesCorruptMarker(t *testing.T) {
	tests := map[string]string{
		"invalid json":    "{",
		"invalid version": `{"version":2,"phase":"wedged"}`,
		"invalid phase":   `{"version":1,"phase":"unknown"}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
			if err != nil {
				t.Fatalf("NewPOSIXSupervisor: %v", err)
			}
			markerPath := filepath.Join(supervisor.dir, nativeHelperMarkerName)
			if err := os.WriteFile(markerPath, []byte(contents), 0o600); err != nil {
				t.Fatalf("write corrupt marker: %v", err)
			}
			if err := supervisor.Revalidate(context.Background()); err != nil {
				t.Fatalf("Revalidate: %v", err)
			}
			if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt marker remains: %v", err)
			}
		})
	}
}

func TestRecoveryRemovesUnreadableMarker(t *testing.T) {
	pidText := os.Getenv("MCPD_TEST_UNREADABLE_PID")
	if pidText == "" {
		t.Skip("requires an unreadable process owned by another user")
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("unreadable pid: %v", err)
	}
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	marker := helperMarker{
		Version: 1, Phase: helperPhaseWedged, InstanceID: "unreadable", DaemonInstanceID: "test-daemon",
		OwnerPID: os.Getpid(), HelperPID: pid, SessionID: pid, ProcessGroupID: pid,
		Executable: os.Args[0], HelperStartIdentity: "unreadable",
		OperationDeadline: time.Now().Add(-time.Second), TerminationDeadline: time.Now().Add(time.Second),
	}
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	if err := supervisor.Revalidate(context.Background()); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable stale marker remains: %v", err)
	}
}

func TestRunDoesNotBypassLiveMarker(t *testing.T) {
	root := stateSandbox(t)
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0], "-test.run=^TestPOSIXSupervisorProcessHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	instance, err := randomHelperID()
	if err != nil {
		t.Fatalf("randomHelperID: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXRecoverySleeper$", "--", nativeHelperArg, instance)
	cmd.Env = append(os.Environ(), nativeHelperIDEnv+"="+instance)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start existing helper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = supervisor.removeMarker()
	})
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect existing helper: %v", err)
	}
	marker := markerForIdentity(identity, instance)
	marker.OperationDeadline = time.Now().Add(time.Second)
	marker.TerminationDeadline = time.Now().Add(2 * time.Second)
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(400 * time.Millisecond)}, "")
	assertCondition(t, err, ConditionBusy)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("existing helper was disturbed: %v", err)
	}
}

func TestOrphanedInFlightMarkerRecoversBeforeDeadline(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	instance, err := randomHelperID()
	if err != nil {
		t.Fatalf("randomHelperID: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXRecoverySleeper$", "--", nativeHelperArg, instance)
	cmd.Env = append(os.Environ(), nativeHelperIDEnv+"="+instance)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-wait
		}
	})
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect helper: %v", err)
	}
	marker := markerForIdentity(identity, instance)
	marker.OwnerPID = 999999
	marker.OwnerStartIdentity = "missing-owner"
	marker.OperationDeadline = time.Now().Add(time.Second)
	marker.TerminationDeadline = time.Now().Add(2 * time.Second)
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	if err := supervisor.Revalidate(context.Background()); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-wait
		stopped = true
		var providerErr *Error
		_ = errors.As(err, &providerErr)
		t.Fatalf("Revalidate: %v (cause %v)", err, providerErr.Cause)
	}
	<-wait
	stopped = true
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned marker remains: %v", err)
	}
}

func TestLockContentionReadsLiveMarker(t *testing.T) {
	root := stateSandbox(t)
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	instance, err := randomHelperID()
	if err != nil {
		t.Fatalf("randomHelperID: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXRecoverySleeper$", "--", nativeHelperArg, instance)
	cmd.Env = append(os.Environ(), nativeHelperIDEnv+"="+instance)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start existing helper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = supervisor.removeMarker()
	})
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect existing helper: %v", err)
	}
	marker := markerForIdentity(identity, instance)
	marker.OperationDeadline = time.Now().Add(time.Second)
	marker.TerminationDeadline = time.Now().Add(2 * time.Second)
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	lock, err := os.OpenFile(filepath.Join(supervisor.dir, nativeHelperLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(time.Second)}, "")
	assertCondition(t, err, ConditionBusy)
}

func TestRecoveryNeverSignalsOwnSession(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	identity, err := inspectPOSIXProcess(os.Getpid())
	if err != nil {
		t.Fatalf("inspect caller: %v", err)
	}
	marker := markerForIdentity(identity, "not-the-process-instance")
	marker.Phase = helperPhaseWedged
	marker.SessionID = identity.SessionID
	marker.ProcessGroupID = unix.Getpgrp()
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	if err := supervisor.Revalidate(context.Background()); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("own-group marker remains: %v", err)
	}
}

func TestWedgedHelperSelfRecovers(t *testing.T) {
	supervisor, err := NewPOSIXSupervisor(filepath.Join(stateSandbox(t), "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	instance, err := randomHelperID()
	if err != nil {
		t.Fatalf("randomHelperID: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXRecoverySleeper$", "--", nativeHelperArg, instance)
	cmd.Env = append(os.Environ(), nativeHelperIDEnv+"="+instance)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("inspect helper: %v", err)
	}
	marker := markerForIdentity(identity, instance)
	marker.Phase = helperPhaseWedged
	if err := supervisor.storeMarker(marker); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("storeMarker: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	if err := supervisor.Revalidate(context.Background()); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wedged marker remains after helper exit: %v", err)
	}
}

func TestRecoveryTerminatesGroupAfterLeaderExit(t *testing.T) {
	root := stateSandbox(t)
	childPath := filepath.Join(root, "child-pid")
	releasePath := filepath.Join(root, "release")
	supervisor, err := NewPOSIXSupervisor(filepath.Join(root, "state"), os.Args[0])
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	instance, err := randomHelperID()
	if err != nil {
		t.Fatalf("randomHelperID: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXExitedLeaderHelper$", "--", nativeHelperArg, instance)
	cmd.Env = append(os.Environ(), nativeHelperIDEnv+"="+instance, "MCPD_CHILD_PID="+childPath, "MCPD_RELEASE_LEADER="+releasePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	identity, err := inspectPOSIXProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect helper: %v", err)
	}
	marker := markerForIdentity(identity, instance)
	marker.Phase = helperPhaseWedged
	if err := supervisor.storeMarker(marker); err != nil {
		t.Fatalf("storeMarker: %v", err)
	}
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for childPID == 0 && time.Now().Before(deadline) {
		data, readErr := os.ReadFile(childPath)
		if readErr == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if childPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childPID == 0 {
		t.Fatal("helper did not report descendant pid")
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("descendant did not survive leader exit: %v", err)
	}
	if err := supervisor.Revalidate(context.Background()); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant survived recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supervisor.dir, nativeHelperMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker remains after recovery: %v", err)
	}
}

func TestPOSIXExitedLeaderHelper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	child := exec.Command("sleep", "30")
	child.Env = os.Environ()
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("MCPD_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		os.Exit(2)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("MCPD_RELEASE_LEADER")); err == nil {
			os.Exit(0)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = child.Process.Kill()
	os.Exit(2)
}

func TestPOSIXRecoverySleeper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func markerForIdentity(identity posixProcessIdentity, instance string) helperMarker {
	owner, _ := inspectPOSIXProcess(os.Getpid())
	return helperMarker{
		Version:             1,
		Phase:               helperPhaseInFlight,
		InstanceID:          instance,
		DaemonInstanceID:    "test-daemon",
		OwnerPID:            os.Getpid(),
		OwnerStartIdentity:  owner.StartIdentity,
		HelperPID:           identity.PID,
		SessionID:           identity.SessionID,
		ProcessGroupID:      identity.PGID,
		Executable:          identity.Executable,
		HelperStartIdentity: identity.StartIdentity,
		OperationDeadline:   time.Now().Add(-time.Second),
		TerminationDeadline: time.Now().Add(time.Second),
	}
}
