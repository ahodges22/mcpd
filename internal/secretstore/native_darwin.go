//go:build darwin

package secretstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var darwinReturnedStatus = regexp.MustCompile("returned (-?[0-9]+)")

type darwinSecurityResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

type darwinCommandRunner interface {
	Run(context.Context, string) darwinSecurityResult
}

type darwinAdapter struct {
	runner darwinCommandRunner
}

type darwinSecurityCommandRunner struct {
	markerDir  string
	executable string
	args       []string
}

func newDarwinSecurityCommandRunner(markerDir, executable string, args ...string) (*darwinSecurityCommandRunner, error) {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	if err := ValidateStateDir(markerDir); err != nil {
		return nil, err
	}
	return &darwinSecurityCommandRunner{
		markerDir:  markerDir,
		executable: executable,
		args:       append([]string(nil), args...),
	}, nil
}

func (r *darwinSecurityCommandRunner) Run(ctx context.Context, command string) darwinSecurityResult {
	cmdArgs := append(append([]string(nil), r.args...), "-i")
	cmd := exec.Command(r.executable, cmdArgs...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return darwinSecurityResult{err: err}
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return darwinSecurityResult{err: err}
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	pid := cmd.Process.Pid
	pgid, err := unix.Getpgid(pid)
	if err != nil || pgid != pid {
		stop()
		return darwinSecurityResult{err: fmt.Errorf("security process group confirmation")}
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		stop()
		return darwinSecurityResult{err: fmt.Errorf("security session confirmation")}
	}
	child, err := inspectPOSIXProcess(pid)
	if err != nil {
		stop()
		return darwinSecurityResult{err: err}
	}
	supervisor := &POSIXSupervisor{dir: r.markerDir}
	marker, err := supervisor.readMarker()
	if err != nil {
		stop()
		return darwinSecurityResult{err: err}
	}
	helper, err := inspectPOSIXProcess(os.Getpid())
	if err != nil || !helper.matchesLeader(marker) {
		stop()
		return darwinSecurityResult{err: fmt.Errorf("security parent helper identity mismatch")}
	}
	if child.ParentPID != marker.HelperPID ||
		child.PGID != pid ||
		child.SessionID != marker.SessionID ||
		child.UID != os.Geteuid() ||
		child.Executable != r.executable ||
		!containsString(child.Arguments, "-i") {
		stop()
		return darwinSecurityResult{err: fmt.Errorf("security child identity mismatch")}
	}
	marker.NativeChildPID = child.PID
	marker.NativeChildParentPID = child.ParentPID
	marker.NativeChildSessionID = sid
	marker.NativeChildProcessGroupID = pgid
	marker.NativeChildExecutable = child.Executable
	marker.NativeChildStartIdentity = child.StartIdentity
	if err := supervisor.storeMarker(marker); err != nil {
		stop()
		return darwinSecurityResult{err: err}
	}
	if _, err := io.WriteString(stdin, command); err != nil {
		_ = stdin.Close()
		stop()
		return darwinSecurityResult{err: err}
	}
	if err := stdin.Close(); err != nil {
		stop()
		return darwinSecurityResult{err: err}
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		exitCode := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return darwinSecurityResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode, err: err}
	case <-ctx.Done():
		_ = unix.Kill(pid, unix.SIGTERM)
		select {
		case <-wait:
		case <-time.After(100 * time.Millisecond):
			_ = unix.Kill(-pgid, unix.SIGKILL)
			<-wait
		}
		return darwinSecurityResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), err: ctx.Err()}
	}
}

func (a *darwinAdapter) Execute(ctx context.Context, request HelperRequest, setValue string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, darwinSecurityError(request.Operation, request.Name, darwinSecurityResult{err: err})
	}
	if request.Operation == OperationSet {
		if err := ValidateValue(setValue); err != nil {
			return Result{}, err
		}
	}
	preflight := a.runner.Run(ctx, "default-keychain -d user\n")
	if preflight.err != nil {
		return Result{}, darwinSecurityError(OperationHealth, request.Name, preflight)
	}
	keychain := strings.TrimSpace(string(preflight.stdout))
	keychain, err := strconv.Unquote(keychain)
	if err != nil || !filepath.IsAbs(keychain) {
		return Result{}, &Error{
			Operation: OperationHealth,
			Provider:  "native",
			Name:      request.Name,
			Condition: ConditionUnavailable,
			Cause:     fmt.Errorf("invalid user default keychain"),
		}
	}
	if request.Operation == OperationHealth {
		return Result{}, nil
	}

	var command string
	switch request.Operation {
	case OperationGet:
		command = fmt.Sprintf("find-generic-password -a %s -s %s -w\n", request.Name, nativeServiceName)
	case OperationSet:
		command = fmt.Sprintf("add-generic-password -a %s -s %s -U -X %s\n", request.Name, nativeServiceName, hex.EncodeToString([]byte(setValue)))
	case OperationDelete:
		command = fmt.Sprintf("delete-generic-password -a %s -s %s\n", request.Name, nativeServiceName)
	default:
		return Result{}, &Error{
			Operation: request.Operation,
			Provider:  "native",
			Name:      request.Name,
			Condition: ConditionUnexpected,
			Cause:     fmt.Errorf("unsupported native operation"),
		}
	}
	result := a.runner.Run(ctx, command)
	if result.err != nil {
		operationErr := darwinSecurityError(request.Operation, request.Name, result)
		if condition, _ := ConditionOf(operationErr); condition == ConditionNotFound {
			if request.Operation == OperationGet {
				return Result{}, nil
			}
			if request.Operation == OperationDelete {
				return Result{}, nil
			}
		}
		return Result{}, operationErr
	}
	if request.Operation != OperationGet {
		return Result{}, nil
	}
	value := bytes.TrimSuffix(result.stdout, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	if err := ValidateValue(string(value)); err != nil {
		return Result{}, &Error{
			Operation: OperationGet,
			Provider:  "native",
			Name:      request.Name,
			Condition: ConditionInvalidValue,
			Cause:     err,
		}
	}
	return Result{Value: string(value), Present: true}, nil
}

func darwinSecurityError(operation Operation, name string, result darwinSecurityResult) error {
	condition := ConditionUnexpected
	if errors.Is(result.err, context.DeadlineExceeded) || errors.Is(result.err, context.Canceled) {
		condition = ConditionInteraction
	} else if match := darwinReturnedStatus.FindSubmatch(result.stderr); len(match) == 2 {
		status, _ := strconv.Atoi(string(match[1]))
		switch status {
		case -25300:
			if operation == OperationGet || operation == OperationDelete {
				condition = ConditionNotFound
			} else {
				condition = ConditionUnavailable
			}
		case -25308, -128:
			condition = ConditionInteraction
		case -25293:
			condition = ConditionLocked
		case -34018:
			condition = ConditionDenied
		}
	}
	return &Error{
		Operation: operation,
		Provider:  "native",
		Name:      name,
		Condition: condition,
		Cause:     result.err,
	}
}
