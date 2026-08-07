//go:build darwin

package secretstore

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (s *POSIXSupervisor) signalNativeChild(marker helperMarker, signal unix.Signal) (bool, error) {
	if marker.NativeChildPID <= 1 {
		return true, nil
	}
	ownSID, _ := unix.Getsid(0)
	if marker.NativeChildProcessGroupID == unix.Getpgrp() ||
		marker.NativeChildSessionID == ownSID ||
		marker.NativeChildProcessGroupID != marker.NativeChildPID ||
		marker.NativeChildSessionID != marker.SessionID {
		return false, nil
	}
	leader, err := inspectPOSIXProcess(marker.NativeChildPID)
	leaderGone := processGone(err)
	if !leaderGone && (identityUnprovable(err) || errors.Is(err, errProcessIdentityTransient)) {
		return false, nil
	}
	if err != nil && !leaderGone {
		return false, nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
	}
	if !leaderGone && !matchesNativeChild(leader, marker) {
		return false, nil
	}
	if signal == unix.SIGTERM && !leaderGone {
		return true, nilIfProcessGone(unix.Kill(marker.NativeChildPID, signal))
	}
	members, err := inspectPOSIXGroup(marker.NativeChildProcessGroupID)
	if identityUnprovable(err) || errors.Is(err, errProcessIdentityTransient) {
		return false, nil
	}
	if err != nil {
		return false, nativeSlotError(OperationHealth, "", ConditionUnexpected, err)
	}
	allProven := len(members) > 0
	for _, member := range members {
		if !matchesNativeChildMember(member, marker) {
			allProven = false
		}
	}
	if !leaderGone && allProven {
		return true, nilIfProcessGone(unix.Kill(-marker.NativeChildProcessGroupID, signal))
	}
	for _, member := range members {
		if matchesNativeChildMember(member, marker) {
			_ = unix.Kill(member.PID, signal)
		}
	}
	return len(members) == 0, nil
}

func matchesNativeChild(process posixProcessIdentity, marker helperMarker) bool {
	executable, err := filepath.EvalSymlinks(marker.NativeChildExecutable)
	if err != nil {
		return false
	}
	parentMatches := process.ParentPID == marker.NativeChildParentPID
	if !parentMatches {
		if _, err := inspectPOSIXProcess(marker.HelperPID); processGone(err) {
			parentMatches = process.ParentPID == 1
		}
	}
	return process.PID == marker.NativeChildPID &&
		process.PGID == marker.NativeChildProcessGroupID &&
		process.SessionID == marker.NativeChildSessionID &&
		process.UID == os.Geteuid() &&
		process.StartIdentity == marker.NativeChildStartIdentity &&
		process.Executable == executable &&
		parentMatches &&
		containsString(process.Arguments, "-i")
}

func matchesNativeChildMember(process posixProcessIdentity, marker helperMarker) bool {
	return process.PGID == marker.NativeChildProcessGroupID &&
		process.SessionID == marker.NativeChildSessionID &&
		process.UID == os.Geteuid() &&
		(!process.EnvironmentVisible || containsString(process.Environment, nativeHelperIDEnv+"="+marker.InstanceID))
}
