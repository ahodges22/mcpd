//go:build darwin || linux

package secretstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const nativeHelperIDEnv = "MCPD_NATIVE_HELPER_ID"
const nativeHelperMarkerName = "native-helper.json"
const nativeHelperArg = "--mcpd-native-helper"

type helperPhase string

const (
	helperPhaseInFlight helperPhase = "in-flight"
	helperPhaseWedged   helperPhase = "wedged"
)

type helperMarker struct {
	Version             int         `json:"version"`
	Phase               helperPhase `json:"phase"`
	InstanceID          string      `json:"instance_id"`
	DaemonInstanceID    string      `json:"daemon_instance_id"`
	OwnerPID            int         `json:"owner_pid"`
	OwnerStartIdentity  string      `json:"owner_start_identity"`
	HelperPID           int         `json:"helper_pid"`
	SessionID           int         `json:"session_id"`
	ProcessGroupID      int         `json:"process_group_id"`
	Executable          string      `json:"executable"`
	HelperStartIdentity string      `json:"helper_start_identity"`
	OperationDeadline   time.Time   `json:"operation_deadline"`
	TerminationDeadline time.Time   `json:"termination_deadline"`
}

type POSIXSupervisor struct {
	dir              string
	executable       string
	args             []string
	extraEnv         []string
	daemonInstanceID string
	terminationWait  time.Duration
	writeMarker      func(helperMarker) error
}

func NewPOSIXSupervisor(stateDir, executable string, args ...string) (*POSIXSupervisor, error) {
	slot, err := NewNativeSlot(stateDir)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	instance, err := randomHelperID()
	if err != nil {
		return nil, err
	}
	s := &POSIXSupervisor{
		dir:              slot.dir,
		executable:       executable,
		args:             append([]string(nil), args...),
		daemonInstanceID: instance,
		terminationWait:  2 * time.Second,
	}
	s.writeMarker = s.storeMarker
	return s, nil
}

func (s *POSIXSupervisor) Run(ctx context.Context, request HelperRequest, setValue string) (Result, error) {
	if err := validateHelperRequest(request, setValue); err != nil {
		return Result{}, err
	}
	operationCtx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	slot := &NativeSlot{dir: s.dir}
	lease, err := slot.Acquire(operationCtx, request.Operation, request.Name)
	if err != nil {
		var providerErr *Error
		if errors.As(err, &providerErr) && providerErr.Condition == ConditionLockContended {
			if markerErr := s.contendedMarkerError(request.Operation, request.Name); markerErr != nil {
				return Result{}, markerErr
			}
		}
		return Result{}, err
	}
	defer lease.Release()
	if err := s.revalidateLocked(); err != nil {
		return Result{}, err
	}

	instance, err := randomHelperID()
	if err != nil {
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	requestRead, requestWrite, err := os.Pipe()
	if err != nil {
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		requestRead.Close()
		requestWrite.Close()
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	defer requestWrite.Close()
	defer responseRead.Close()

	cmdArgs := append(append([]string(nil), s.args...), "--", nativeHelperArg, instance)
	cmd := exec.Command(s.executable, cmdArgs...)
	cmd.Env = append(append(os.Environ(), s.extraEnv...), nativeHelperIDEnv+"="+instance)
	cmd.Stdin = requestRead
	cmd.Stdout = responseWrite
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		requestRead.Close()
		responseWrite.Close()
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	requestRead.Close()
	responseWrite.Close()
	pid := cmd.Process.Pid
	pgid, err := unix.Getpgid(pid)
	if err != nil || pgid != pid {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, fmt.Errorf("helper process group confirmation: pgid=%d pid=%d: %w", pgid, pid, err))
	}
	sid, err := unix.Getsid(pid)
	if err != nil || sid != pid {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, fmt.Errorf("helper session confirmation: sid=%d pid=%d: %w", sid, pid, err))
	}
	helperIdentity, err := inspectPOSIXProcess(pid)
	if err != nil {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	ownerIdentity, err := inspectPOSIXProcess(os.Getpid())
	if err != nil {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}
	operationDeadline, _ := operationCtx.Deadline()
	marker := helperMarker{
		Version:             1,
		Phase:               helperPhaseInFlight,
		InstanceID:          instance,
		DaemonInstanceID:    s.daemonInstanceID,
		OwnerPID:            os.Getpid(),
		OwnerStartIdentity:  ownerIdentity.StartIdentity,
		HelperPID:           pid,
		SessionID:           sid,
		ProcessGroupID:      pgid,
		Executable:          s.executable,
		HelperStartIdentity: helperIdentity.StartIdentity,
		OperationDeadline:   operationDeadline,
		TerminationDeadline: operationDeadline.Add(s.terminationWait),
	}
	if err := s.writeMarker(marker); err != nil {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, errors.Join(err, s.removeMarker()))
	}
	if err := WriteHelperRequest(requestWrite, request, setValue); err != nil {
		requestWrite.Close()
		s.terminateStarted(cmd, pid)
		_ = s.removeMarker()
		return Result{}, err
	}
	if err := requestWrite.Close(); err != nil {
		s.terminateStarted(cmd, pid)
		_ = s.removeMarker()
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionUnexpected, err)
	}

	type responseResult struct {
		response HelperResponse
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		response, err := ReadHelperResponse(responseRead)
		responseCh <- responseResult{response: response, err: err}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var response responseResult
	var waitErr error
	gotResponse := false
	gotWait := false
	for !gotResponse || !gotWait {
		select {
		case response = <-responseCh:
			gotResponse = true
			responseCh = nil
		case waitErr = <-waitCh:
			gotWait = true
			waitCh = nil
		case <-operationCtx.Done():
			return s.timeoutOperation(operationCtx.Err(), request, marker, waitCh, gotWait)
		}
	}
	terminated, terminateErr := s.boundedTerminateMarker(marker)
	if terminateErr != nil {
		marker.Phase = helperPhaseWedged
		if err := s.storeMarker(marker); err != nil {
			return Result{}, operationCleanupError(request, errors.Join(terminateErr, err))
		}
		return Result{}, operationCleanupError(request, terminateErr)
	}
	if !terminated {
		marker.Phase = helperPhaseWedged
		if err := s.storeMarker(marker); err != nil {
			return Result{}, operationCleanupError(request, err)
		}
		condition := ConditionWedged
		if request.Operation == OperationSet || request.Operation == OperationDelete {
			condition = ConditionIndeterminate
		}
		return Result{}, nativeSlotError(request.Operation, request.Name, condition, fmt.Errorf("helper process group identity is incomplete"))
	}
	if err := s.removeMarker(); err != nil {
		return Result{}, operationCleanupError(request, err)
	}
	if response.err != nil {
		return Result{}, operationCleanupError(request, response.err)
	}
	if waitErr != nil {
		return Result{}, operationCleanupError(request, waitErr)
	}
	return response.response.Result, response.response.Err()
}

func (s *POSIXSupervisor) timeoutOperation(cause error, request HelperRequest, marker helperMarker, waitCh <-chan error, alreadyWaited bool) (Result, error) {
	terminated, terminateErr := s.boundedTerminateMarker(marker)
	if terminateErr != nil {
		marker.Phase = helperPhaseWedged
		if err := s.storeMarker(marker); err != nil {
			return Result{}, operationCleanupError(request, errors.Join(terminateErr, err))
		}
		return Result{}, operationCleanupError(request, terminateErr)
	}
	if !terminated {
		marker.Phase = helperPhaseWedged
		if err := s.storeMarker(marker); err != nil {
			return Result{}, operationCleanupError(request, err)
		}
		return Result{}, nativeSlotError(request.Operation, request.Name, ConditionWedged, fmt.Errorf("helper process group identity is incomplete"))
	}
	if !alreadyWaited && waitCh != nil {
		select {
		case <-waitCh:
		default:
		}
	}
	if err := s.removeMarker(); err != nil {
		return Result{}, operationCleanupError(request, err)
	}
	condition := ConditionTimedOut
	if request.Operation == OperationSet || request.Operation == OperationDelete {
		condition = ConditionIndeterminate
	}
	return Result{}, nativeSlotError(request.Operation, request.Name, condition, cause)
}

func operationCleanupError(request HelperRequest, cause error) error {
	condition := ConditionUnexpected
	if request.Operation == OperationSet || request.Operation == OperationDelete {
		condition = ConditionIndeterminate
	}
	return nativeSlotError(request.Operation, request.Name, condition, cause)
}

func (s *POSIXSupervisor) contendedMarkerError(operation Operation, name string) error {
	marker, err := s.readMarker()
	if err != nil {
		return nil
	}
	ownSID, _ := unix.Getsid(0)
	if marker.ProcessGroupID == unix.Getpgrp() || marker.SessionID == ownSID || marker.HelperPID <= 1 || marker.ProcessGroupID != marker.HelperPID || marker.SessionID != marker.HelperPID {
		return nil
	}
	leader, err := inspectPOSIXProcess(marker.HelperPID)
	if err != nil || !leader.matchesLeader(marker) {
		return nil
	}
	condition := ConditionBusy
	if marker.Phase == helperPhaseWedged {
		condition = ConditionWedged
	}
	return nativeSlotError(operation, name, condition, fmt.Errorf("matching helper marker while native lock is held"))
}

func (s *POSIXSupervisor) Revalidate(ctx context.Context) error {
	slot := &NativeSlot{dir: s.dir}
	lease, err := slot.Acquire(ctx, OperationHealth, "")
	if err != nil {
		return err
	}
	defer lease.Release()
	return s.revalidateLocked()
}

func (s *POSIXSupervisor) revalidateLocked() error {
	marker, err := s.readMarker()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if condition, ok := ConditionOf(err); ok && condition == ConditionCorrupt {
		return s.removeMarker()
	}
	if err != nil {
		return err
	}
	ownSID, _ := unix.Getsid(0)
	if marker.ProcessGroupID == unix.Getpgrp() || marker.SessionID == ownSID || marker.HelperPID <= 1 || marker.ProcessGroupID != marker.HelperPID || marker.SessionID != marker.HelperPID {
		return s.removeMarker()
	}
	leader, err := inspectPOSIXProcess(marker.HelperPID)
	leaderGone := processGone(err)
	if identityUnprovable(err) {
		return s.removeMarker()
	}
	if err != nil && !leaderGone {
		return nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
	}
	if !leaderGone && !leader.matchesLeader(marker) {
		return s.removeMarker()
	}
	if !leaderGone && marker.Phase == helperPhaseInFlight && time.Now().Before(marker.OperationDeadline) {
		ownerLive, err := markerOwnerLive(marker)
		if err != nil {
			return nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
		}
		if ownerLive {
			return nativeSlotError(OperationHealth, "", ConditionBusy, fmt.Errorf("helper operation is in flight"))
		}
	}
	terminated, err := s.boundedTerminateMarker(marker)
	if err != nil {
		return err
	}
	if !terminated {
		marker.Phase = helperPhaseWedged
		if err := s.storeMarker(marker); err != nil {
			return err
		}
		return nativeSlotError(OperationHealth, "", ConditionWedged, fmt.Errorf("process group member identity is unproven"))
	}
	return s.removeMarker()
}

func markerOwnerLive(marker helperMarker) (bool, error) {
	if marker.OwnerPID <= 1 || marker.OwnerStartIdentity == "" {
		return false, nil
	}
	owner, err := inspectPOSIXProcess(marker.OwnerPID)
	if processGone(err) || identityUnprovable(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return owner.UID == os.Geteuid() && owner.StartIdentity == marker.OwnerStartIdentity, nil
}

func (s *POSIXSupervisor) terminateMarkerGroup(marker helperMarker) (bool, error) {
	return s.signalMarkerGroup(marker, unix.SIGKILL)
}

func (s *POSIXSupervisor) signalMarkerGroup(marker helperMarker, signal unix.Signal) (bool, error) {
	ownSID, _ := unix.Getsid(0)
	if marker.ProcessGroupID == unix.Getpgrp() || marker.SessionID == ownSID || marker.HelperPID <= 1 || marker.ProcessGroupID != marker.HelperPID || marker.SessionID != marker.HelperPID {
		return false, nil
	}
	leader, err := inspectPOSIXProcess(marker.HelperPID)
	leaderGone := processGone(err)
	if !leaderGone && (identityUnprovable(err) || errors.Is(err, errProcessIdentityTransient)) {
		return false, nil
	}
	if err != nil && !leaderGone {
		return false, nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
	}
	if !leaderGone && !leader.matchesLeader(marker) {
		return false, nil
	}
	members, err := inspectPOSIXGroup(marker.ProcessGroupID)
	if identityUnprovable(err) || errors.Is(err, errProcessIdentityTransient) {
		return false, nil
	}
	if err != nil {
		return false, nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
	}
	allProven := len(members) > 0
	for _, member := range members {
		if !member.matchesGroupMember(marker) {
			allProven = false
		}
	}
	if !leaderGone && allProven {
		err := unix.Kill(-marker.ProcessGroupID, signal)
		return true, nilIfProcessGone(err)
	}
	for _, member := range members {
		if member.matchesGroupMember(marker) {
			_ = unix.Kill(member.PID, signal)
		}
	}
	return false, nil
}

func (s *POSIXSupervisor) boundedTerminateMarker(marker helperMarker) (bool, error) {
	now := time.Now()
	maximumDeadline := now.Add(s.terminationWait)
	deadline := marker.TerminationDeadline
	if !deadline.After(now) || deadline.After(maximumDeadline) {
		deadline = maximumDeadline
	}
	_, err := s.signalMarkerGroup(marker, unix.SIGTERM)
	if err != nil {
		return false, err
	}
	remaining := time.Until(deadline)
	grace := remaining / 2
	if grace > 100*time.Millisecond {
		grace = 100 * time.Millisecond
	}
	if grace > 0 {
		exited, err := waitForPOSIXGroupExit(marker.ProcessGroupID, time.Now().Add(grace))
		if err != nil {
			return false, err
		}
		if exited {
			return true, nil
		}
	}
	_, err = s.signalMarkerGroup(marker, unix.SIGKILL)
	if err != nil {
		return false, err
	}
	return waitForPOSIXGroupExit(marker.ProcessGroupID, deadline)
}

func waitForPOSIXGroupExit(pgid int, deadline time.Time) (bool, error) {
	for {
		members, err := inspectPOSIXGroup(pgid)
		if err == nil && len(members) == 0 {
			return true, nil
		}
		if err != nil && !errors.Is(err, errProcessIdentityTransient) {
			return false, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nilIfProcessGone(err error) error {
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (s *POSIXSupervisor) terminateStarted(cmd *exec.Cmd, pgid int) {
	_ = unix.Kill(-pgid, unix.SIGKILL)
	_ = cmd.Wait()
}

func (s *POSIXSupervisor) storeMarker(marker helperMarker) error {
	if err := ValidateStateDir(s.dir); err != nil {
		return err
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := restrictedTemp(s.dir, ".native-helper-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.dir, nativeHelperMarkerName)); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *POSIXSupervisor) readMarker() (helperMarker, error) {
	if err := ValidateStateDir(s.dir); err != nil {
		return helperMarker{}, err
	}
	path := filepath.Join(s.dir, nativeHelperMarkerName)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return helperMarker{}, os.ErrNotExist
	}
	if err != nil {
		return helperMarker{}, nativeSlotError(OperationValidate, nativeHelperMarkerName, ConditionPermission, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := validatePOSIXArtifact("native", nativeHelperMarkerName, file); err != nil {
		return helperMarker{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024))
	if err != nil {
		return helperMarker{}, err
	}
	var marker helperMarker
	if err := json.Unmarshal(data, &marker); err != nil || marker.Version != 1 || (marker.Phase != helperPhaseInFlight && marker.Phase != helperPhaseWedged) {
		return helperMarker{}, nativeSlotError(OperationValidate, nativeHelperMarkerName, ConditionCorrupt, fmt.Errorf("invalid helper marker"))
	}
	return marker, nil
}

func (s *POSIXSupervisor) removeMarker() error {
	if err := ValidateStateDir(s.dir); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.dir, nativeHelperMarkerName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(s.dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}

func randomHelperID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
