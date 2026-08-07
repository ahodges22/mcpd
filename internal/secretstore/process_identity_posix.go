//go:build darwin || linux

package secretstore

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var errProcessIdentityTransient = errors.New("process identity temporarily unavailable")

type posixProcessIdentity struct {
	PID                int
	ParentPID          int
	PGID               int
	SessionID          int
	UID                int
	StartIdentity      string
	Executable         string
	Arguments          []string
	Environment        []string
	EnvironmentVisible bool
}

func (p posixProcessIdentity) matchesLeader(marker helperMarker) bool {
	if p.PID != marker.HelperPID || p.PGID != marker.ProcessGroupID || p.SessionID != marker.SessionID || p.UID != os.Geteuid() || p.StartIdentity != marker.HelperStartIdentity {
		return false
	}
	wantExecutable, err := filepath.EvalSymlinks(marker.Executable)
	if err != nil || p.Executable != wantExecutable {
		return false
	}
	return containsString(p.Arguments, nativeHelperArg) && containsString(p.Arguments, marker.InstanceID) && containsString(p.Environment, nativeHelperIDEnv+"="+marker.InstanceID)
}

func (p posixProcessIdentity) matchesGroupMember(marker helperMarker) bool {
	if p.PGID != marker.ProcessGroupID || p.SessionID != marker.SessionID || p.UID != os.Geteuid() {
		return false
	}
	return !p.EnvironmentVisible || containsString(p.Environment, nativeHelperIDEnv+"="+marker.InstanceID)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrProcessDone) || errors.Is(err, unix.ESRCH)
}

func identityUnprovable(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}
