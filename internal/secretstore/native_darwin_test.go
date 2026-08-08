//go:build darwin

package secretstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinErrorMapping(t *testing.T) {
	tests := []struct {
		name      string
		operation Operation
		result    darwinSecurityResult
		want      Condition
	}{
		{name: "missing item", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -25300"), exitCode: 44, err: errors.New("exit status 44")}, want: ConditionNotFound},
		{name: "missing default keychain", operation: OperationHealth, result: darwinSecurityResult{stderr: []byte("default-keychain: returned -25300"), exitCode: 44, err: errors.New("exit status 44")}, want: ConditionUnavailable},
		{name: "interaction prohibited", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -25308"), exitCode: 36, err: errors.New("exit status 36")}, want: ConditionInteraction},
		{name: "user canceled", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -128"), exitCode: 128, err: errors.New("exit status 128")}, want: ConditionInteraction},
		{name: "locked", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -25293"), exitCode: 51, err: errors.New("exit status 51")}, want: ConditionLocked},
		{name: "missing entitlement", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -34018"), exitCode: 30, err: errors.New("exit status 30")}, want: ConditionDenied},
		{name: "deadline", operation: OperationGet, result: darwinSecurityResult{err: context.DeadlineExceeded}, want: ConditionInteraction},
		{name: "unknown", operation: OperationGet, result: darwinSecurityResult{stderr: []byte("find-generic-password: returned -1"), exitCode: 1, err: errors.New("exit status 1")}, want: ConditionUnexpected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := darwinSecurityError(test.operation, "TOKEN", test.result)
			if condition, ok := ConditionOf(err); !ok || condition != test.want {
				t.Fatalf("condition = %q, %v; want %q; error: %v", condition, ok, test.want, err)
			}
		})
	}
}

func TestDarwinNativeChildMemberRequiresVisibleInstanceTag(t *testing.T) {
	marker := helperMarker{
		InstanceID:                "instance",
		NativeChildSessionID:      41,
		NativeChildProcessGroupID: 42,
	}
	identity := posixProcessIdentity{
		PGID:               42,
		SessionID:          41,
		UID:                os.Geteuid(),
		EnvironmentVisible: true,
	}
	if matchesNativeChildMember(identity, marker) {
		t.Fatal("visible environment without instance tag matched native child")
	}
	identity.Environment = []string{nativeHelperIDEnv + "=instance"}
	if !matchesNativeChildMember(identity, marker) {
		t.Fatal("visible environment with instance tag did not match native child")
	}
	identity.Environment = nil
	identity.EnvironmentVisible = false
	if !matchesNativeChildMember(identity, marker) {
		t.Fatal("hidden environment prevented private-session identity proof")
	}
}

type recordingDarwinRunner struct {
	results  []darwinSecurityResult
	commands []string
}

func (r *recordingDarwinRunner) Run(_ context.Context, command string) darwinSecurityResult {
	r.commands = append(r.commands, command)
	return r.next()
}

func (r *recordingDarwinRunner) next() darwinSecurityResult {
	if len(r.results) == 0 {
		return darwinSecurityResult{}
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result
}

func TestDarwinAdapterUsesStdinSafeAtomicCommands(t *testing.T) {
	const value = "  quotes ' \" slash \\ dollar $ tick \x60 café  "
	runner := &recordingDarwinRunner{results: []darwinSecurityResult{
		{stdout: []byte("    \"/Users/test/Library/Keychains/login.keychain-db\"\n")},
		{},
	}}
	adapter := darwinAdapter{runner: runner}
	_, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationSet, Name: "API_TOKEN"}, value)
	if err != nil {
		t.Fatalf("Execute set: %v", err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("commands = %q, want preflight and one set", runner.commands)
	}
	if runner.commands[0] != "default-keychain -d user\n" {
		t.Fatalf("preflight command = %q", runner.commands[0])
	}
	setCommand := runner.commands[1]
	stored := darwinValueEnvelope + base64.RawURLEncoding.EncodeToString([]byte(value))
	if setCommand != "add-generic-password -a API_TOKEN -s io.mcpd.secrets -U -w "+stored+"\n" {
		t.Fatalf("set command = %q", setCommand)
	}
	if strings.Contains(setCommand, value) || strings.Contains(setCommand, "delete-generic-password") {
		t.Fatalf("set command is not stdin-safe and atomic: %q", setCommand)
	}
}

func TestDarwinAdapterRoundTripsHexLikeAndUnicodeValues(t *testing.T) {
	for _, value := range []string{"c3a9", "  café 雪  ", strings.Repeat("x", MaxValueBytes)} {
		runner := &recordingDarwinRunner{results: []darwinSecurityResult{
			{stdout: []byte("\"/Users/test/Library/Keychains/login.keychain-db\"\n")},
			{},
		}}
		adapter := darwinAdapter{runner: runner}
		if _, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationSet, Name: "API_TOKEN"}, value); err != nil {
			t.Fatalf("Execute set %q: %v", value, err)
		}
		fields := strings.Fields(runner.commands[1])
		stored := darwinValueEnvelope + base64.RawURLEncoding.EncodeToString([]byte(value))
		if len(fields) == 0 || fields[len(fields)-1] != stored {
			t.Fatalf("password input does not preserve %q", value)
		}
		runner.results = append(runner.results,
			darwinSecurityResult{stdout: []byte("\"/Users/test/Library/Keychains/login.keychain-db\"\n")},
			darwinSecurityResult{stdout: append([]byte(stored), '\n')},
		)
		got, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationGet, Name: "API_TOKEN"}, "")
		if err != nil {
			t.Fatalf("Execute get %q: %v", value, err)
		}
		if !got.Present || got.Value != value {
			t.Fatalf("Get = %#v, want %q", got, value)
		}
	}
}
func TestDarwinAdapterGetAndDeleteTreatOnlyItemNotFoundAsCleanMiss(t *testing.T) {
	missing := darwinSecurityResult{stderr: []byte("find-generic-password: returned -25300"), exitCode: 44, err: errors.New("exit status 44")}
	runner := &recordingDarwinRunner{results: []darwinSecurityResult{
		{stdout: []byte("\"/Users/test/Library/Keychains/login.keychain-db\"\n")},
		{stdout: []byte("  exact value  \n")},
		{stdout: []byte("\"/Users/test/Library/Keychains/login.keychain-db\"\n")},
		missing,
		{stdout: []byte("\"/Users/test/Library/Keychains/login.keychain-db\"\n")},
		{stderr: []byte("delete-generic-password: returned -25300"), exitCode: 44, err: errors.New("exit status 44")},
	}}
	adapter := darwinAdapter{runner: runner}

	got, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationGet, Name: "API_TOKEN"}, "")
	if err != nil {
		t.Fatalf("Execute get: %v", err)
	}
	if !got.Present || got.Value != "  exact value  " {
		t.Fatalf("get = %#v", got)
	}
	got, err = adapter.Execute(context.Background(), HelperRequest{Operation: OperationGet, Name: "MISSING"}, "")
	if err != nil {
		t.Fatalf("Execute missing get: %v", err)
	}
	if got.Present || got.Value != "" {
		t.Fatalf("missing get = %#v", got)
	}
	if _, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationDelete, Name: "MISSING"}, ""); err != nil {
		t.Fatalf("Execute missing delete: %v", err)
	}
}

type scriptedNativeRunner struct {
	results []struct {
		result Result
		err    error
	}
	requests []HelperRequest
}

func (r *scriptedNativeRunner) Run(_ context.Context, request HelperRequest, _ string) (Result, error) {
	r.requests = append(r.requests, request)
	if len(r.results) == 0 {
		return Result{}, errors.New("unexpected native operation")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.result, result.err
}

func TestDarwinInteractionSuspendsAutomaticReads(t *testing.T) {
	runner := &scriptedNativeRunner{results: []struct {
		result Result
		err    error
	}{
		{err: &Error{Operation: OperationGet, Provider: "native", Name: "TOKEN", Condition: ConditionTimedOut}},
		{result: Result{Value: "must-not-run", Present: true}},
	}}
	store := newDarwinStore(runner)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := store.Get(ctx, "TOKEN"); err == nil {
		t.Fatal("first Get succeeded despite interaction result")
	} else {
		assertCondition(t, err, ConditionInteraction)
	}
	if _, err := store.Get(ctx, "TOKEN"); err == nil {
		t.Fatal("automatic Get bypassed interaction latch")
	} else {
		assertCondition(t, err, ConditionInteraction)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("native requests = %d, want 1", len(runner.requests))
	}
}

func TestDarwinRetryAllowsOneAttempt(t *testing.T) {
	runner := &scriptedNativeRunner{results: []struct {
		result Result
		err    error
	}{
		{err: &Error{Operation: OperationGet, Provider: "native", Name: "TOKEN", Condition: ConditionInteraction}},
		{err: &Error{Operation: OperationGet, Provider: "native", Name: "TOKEN", Condition: ConditionInteraction}},
		{result: Result{Value: "must-not-run", Present: true}},
	}}
	store := newDarwinStore(runner)
	var _ NativeProvider = store
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, _ = store.Get(ctx, "TOKEN")
	store.Retry()
	if _, err := store.Get(ctx, "TOKEN"); err == nil {
		t.Fatal("retry attempt succeeded despite interaction result")
	}
	if _, err := store.Get(ctx, "TOKEN"); err == nil {
		t.Fatal("second automatic Get bypassed relatched interaction")
	} else {
		assertCondition(t, err, ConditionInteraction)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("native requests = %d, want initial attempt plus one retry", len(runner.requests))
	}
}

func TestDarwinSecurityChildRecordedBeforeInput(t *testing.T) {
	root := stateSandbox(t)
	state := filepath.Join(root, "state")
	received := filepath.Join(root, "received")
	diagnostic := filepath.Join(root, "diagnostic")
	supervisor, err := NewPOSIXSupervisor(state, os.Args[0], "-test.run=^TestDarwinRecordedChildHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv,
		"MCPD_TEST_MARKER_DIR="+supervisor.dir,
		"MCPD_TEST_SECURITY_RECEIVED="+received,
		"MCPD_TEST_SECURITY_DIAGNOSTIC="+diagnostic,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationHealth, Deadline: time.Now().Add(8 * time.Second)}, "")
	if err != nil {
		detail, _ := os.ReadFile(diagnostic)
		t.Fatalf("Run: %v; helper detail: %s", err, detail)
	}
	command, err := os.ReadFile(received)
	if err != nil {
		t.Fatalf("read received command: %v", err)
	}
	if string(command) != "default-keychain -d user\n" {
		t.Fatalf("security command = %q", command)
	}
}

func TestDarwinSupervisorTimeoutTerminatesRecordedChild(t *testing.T) {
	root := stateSandbox(t)
	state := filepath.Join(root, "state")
	received := filepath.Join(root, "received")
	diagnostic := filepath.Join(root, "diagnostic")
	childPIDPath := filepath.Join(root, "child-pid")
	supervisor, err := NewPOSIXSupervisor(state, os.Args[0], "-test.run=^TestDarwinRecordedChildHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.terminationWait = 300 * time.Millisecond
	supervisor.extraEnv = append(supervisor.extraEnv,
		"MCPD_TEST_MARKER_DIR="+supervisor.dir,
		"MCPD_TEST_SECURITY_RECEIVED="+received,
		"MCPD_TEST_SECURITY_DIAGNOSTIC="+diagnostic,
		"MCPD_TEST_SECURITY_CHILD_PID="+childPIDPath,
		"MCPD_TEST_SECURITY_STOP_HELPER=1",
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationHealth, Deadline: time.Now().Add(250 * time.Millisecond)}, "")
	assertCondition(t, err, ConditionTimedOut)
	data, readErr := os.ReadFile(childPIDPath)
	if readErr != nil {
		t.Fatalf("read security child pid: %v", readErr)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("security child pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-childPID, syscall.SIGKILL) })
	if _, inspectErr := inspectPOSIXProcess(childPID); !processGone(inspectErr) {
		t.Fatalf("recorded security child survived timeout: %v", inspectErr)
	}
}

func TestDarwinHelperExitTerminatesRecordedChild(t *testing.T) {
	root := stateSandbox(t)
	state := filepath.Join(root, "state")
	received := filepath.Join(root, "received")
	diagnostic := filepath.Join(root, "diagnostic")
	childPIDPath := filepath.Join(root, "child-pid")
	supervisor, err := NewPOSIXSupervisor(state, os.Args[0], "-test.run=^TestDarwinRecordedChildHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.terminationWait = 300 * time.Millisecond
	supervisor.extraEnv = append(supervisor.extraEnv,
		"MCPD_TEST_MARKER_DIR="+supervisor.dir,
		"MCPD_TEST_SECURITY_RECEIVED="+received,
		"MCPD_TEST_SECURITY_DIAGNOSTIC="+diagnostic,
		"MCPD_TEST_SECURITY_CHILD_PID="+childPIDPath,
		"MCPD_TEST_SECURITY_KILL_HELPER=1",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationHealth, Deadline: time.Now().Add(time.Second)}, "")
	assertCondition(t, err, ConditionUnexpected)
	data, readErr := os.ReadFile(childPIDPath)
	if readErr != nil {
		t.Fatalf("read security child pid: %v", readErr)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("security child pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-childPID, syscall.SIGKILL) })
	if _, inspectErr := inspectPOSIXProcess(childPID); !processGone(inspectErr) {
		t.Fatalf("recorded security child survived helper exit: %v", inspectErr)
	}
}

func TestDarwinSetSecretAbsentFromDescendantMetadata(t *testing.T) {
	const value = "metadata-secret-café"
	root := stateSandbox(t)
	state := filepath.Join(root, "state")
	received := filepath.Join(root, "received")
	diagnostic := filepath.Join(root, "diagnostic")
	treeCountPath := filepath.Join(root, "tree-count")
	supervisor, err := NewPOSIXSupervisor(state, os.Args[0], "-test.run=^TestDarwinRecordedChildHelper$")
	if err != nil {
		t.Fatalf("NewPOSIXSupervisor: %v", err)
	}
	supervisor.extraEnv = append(supervisor.extraEnv,
		"MCPD_TEST_MARKER_DIR="+supervisor.dir,
		"MCPD_TEST_SECURITY_RECEIVED="+received,
		"MCPD_TEST_SECURITY_DIAGNOSTIC="+diagnostic,
		"MCPD_TEST_SECURITY_INSPECT_TREE=1",
		"MCPD_TEST_SECURITY_TREE_COUNT="+treeCountPath,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = supervisor.Run(ctx, HelperRequest{Operation: OperationSet, Name: "API_TOKEN", Deadline: time.Now().Add(8 * time.Second)}, value)
	if err != nil {
		detail, _ := os.ReadFile(diagnostic)
		t.Fatalf("Run set: %v; helper detail: %s", err, detail)
	}
	data, err := os.ReadFile(treeCountPath)
	if err != nil {
		t.Fatalf("read inspected tree count: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || count < 2 {
		t.Fatalf("inspected tree count = %q, %v; want at least direct child and descendant", data, err)
	}
}

func TestDarwinNativeRoundTrip(t *testing.T) {
	if os.Getenv("MCPD_TEST_DARWIN_NATIVE") != "1" {
		t.Skip("set MCPD_TEST_DARWIN_NATIVE=1 to allow a temporary default-keychain item")
	}
	store, err := NewNativeStore(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestDarwinRealNativeHelper$")
	if err != nil {
		t.Fatalf("NewNativeStore: %v", err)
	}
	name := fmt.Sprintf("MCPD_TEST_%d", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = store.Delete(cleanupCtx, name)
	})
	values := []string{"quotes ' \" slash \\ dollar $ tick \x60", "c3a9", "  café 雪  ", strings.Repeat("x", MaxValueBytes)}
	for i, want := range values {
		if err := store.Set(ctx, name, want); err != nil {
			t.Fatalf("Set value %d: %v", i, err)
		}
		got, err := store.Get(ctx, name)
		if err != nil {
			t.Fatalf("Get value %d: %v", i, err)
		}
		if !got.Present || got.Value != want {
			t.Fatalf("Get present = %t, bytes = %d; want present with %d bytes", got.Present, len(got.Value), len(want))
		}
	}
}

func TestDarwinRealNativeHelper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	handled, err := ServeNativeHelperIfRequested(context.Background(), os.Args, os.Stdin, os.Stdout)
	if !handled || err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDarwinRecordedChildHelper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	runner, err := newDarwinSecurityCommandRunner(
		os.Getenv("MCPD_TEST_MARKER_DIR"),
		os.Args[0],
		"-test.run=^TestDarwinFakeSecurity$",
		"--",
	)
	if err != nil {
		os.Exit(2)
	}
	adapter := &darwinAdapter{runner: runner}
	execute := func(ctx context.Context, request HelperRequest, value string) (Result, error) {
		result, operationErr := adapter.Execute(ctx, request, value)
		if operationErr != nil {
			var providerErr *Error
			_ = errors.As(operationErr, &providerErr)
			var cause error
			if providerErr != nil {
				cause = providerErr.Cause
			}
			file, _ := os.OpenFile(os.Getenv("MCPD_TEST_SECURITY_DIAGNOSTIC"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if file != nil {
				_, _ = fmt.Fprintf(file, "helper: %v cause=%v\n", operationErr, cause)
				_ = file.Close()
			}
		}
		return result, operationErr
	}
	if err := ServeHelperOnce(context.Background(), os.Stdin, os.Stdout, execute); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDarwinFakeSecurity(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(os.Stdin, first); err != nil {
		os.Exit(2)
	}
	marker, err := (&POSIXSupervisor{dir: os.Getenv("MCPD_TEST_MARKER_DIR")}).readMarker()
	if err != nil {
		os.Exit(2)
	}
	identity, err := inspectPOSIXProcess(os.Getpid())
	if err != nil ||
		marker.NativeChildPID != identity.PID ||
		marker.NativeChildParentPID != identity.ParentPID ||
		marker.NativeChildSessionID != identity.SessionID ||
		marker.NativeChildProcessGroupID != identity.PGID ||
		marker.NativeChildStartIdentity != identity.StartIdentity ||
		marker.NativeChildExecutable != identity.Executable {
		os.Exit(2)
	}
	remainder, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	command := append(first, remainder...)
	isSet := strings.HasPrefix(string(command), "add-generic-password ")
	recorded := command
	if isSet {
		recorded = []byte("set input received")
	}
	if err := os.WriteFile(os.Getenv("MCPD_TEST_SECURITY_RECEIVED"), recorded, 0o600); err != nil {
		os.Exit(2)
	}
	if os.Getenv("MCPD_TEST_SECURITY_STOP_HELPER") == "1" {
		_ = os.WriteFile(os.Getenv("MCPD_TEST_SECURITY_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		signal.Ignore(syscall.SIGTERM)
		_ = syscall.Kill(os.Getppid(), syscall.SIGSTOP)
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("MCPD_TEST_SECURITY_KILL_HELPER") == "1" {
		_ = os.WriteFile(os.Getenv("MCPD_TEST_SECURITY_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		signal.Ignore(syscall.SIGTERM)
		_ = syscall.Kill(os.Getppid(), syscall.SIGKILL)
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("MCPD_TEST_SECURITY_INSPECT_TREE") == "1" && isSet {
		fields := strings.Fields(string(command))
		if len(fields) == 0 {
			os.Exit(2)
		}
		encoded, ok := strings.CutPrefix(fields[len(fields)-1], darwinValueEnvelope)
		if !ok {
			os.Exit(2)
		}
		secret, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			os.Exit(2)
		}
		descendant := exec.Command("sleep", "1")
		if err := descendant.Start(); err != nil {
			os.Exit(2)
		}
		members, err := inspectPOSIXGroup(os.Getpid())
		if err != nil || len(members) < 2 {
			_ = descendant.Process.Kill()
			_ = descendant.Wait()
			os.Exit(2)
		}
		for _, member := range members {
			metadata := strings.Join(append(append([]string(nil), member.Arguments...), member.Environment...), "\x00")
			if strings.Contains(metadata, string(secret)) {
				_ = descendant.Process.Kill()
				_ = descendant.Wait()
				os.Exit(2)
			}
		}
		_ = descendant.Process.Kill()
		_ = descendant.Wait()
		_ = os.WriteFile(os.Getenv("MCPD_TEST_SECURITY_TREE_COUNT"), []byte(strconv.Itoa(len(members))), 0o600)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("\"/tmp/test.keychain-db\"\n")
	os.Exit(0)
}
