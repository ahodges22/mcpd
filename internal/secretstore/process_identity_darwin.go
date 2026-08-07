//go:build darwin

package secretstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

const darwinZombieState = 5

func inspectPOSIXProcess(pid int) (posixProcessIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info.Proc.P_pid == 0 {
		return posixProcessIdentity{}, os.ErrNotExist
	}
	if info.Proc.P_stat == darwinZombieState {
		return posixProcessIdentity{}, os.ErrNotExist
	}
	if int(info.Eproc.Ucred.Uid) != os.Geteuid() {
		return posixProcessIdentity{}, fmt.Errorf("process %d is owned by uid %d: %w", pid, info.Eproc.Ucred.Uid, unix.EPERM)
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, unix.EINVAL) && errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return posixProcessIdentity{}, os.ErrNotExist
		}
		if errors.Is(err, unix.EINVAL) {
			return posixProcessIdentity{}, fmt.Errorf("%w: process arguments for state %d", errProcessIdentityTransient, info.Proc.P_stat)
		}
		return posixProcessIdentity{}, fmt.Errorf("process arguments for state %d: %w", info.Proc.P_stat, err)
	}
	if len(raw) < 5 {
		return posixProcessIdentity{}, fmt.Errorf("short kern.procargs2")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	parts := bytes.Split(raw[4:], []byte{0})
	if len(parts) == 0 || len(parts[0]) == 0 {
		return posixProcessIdentity{}, fmt.Errorf("missing process executable")
	}
	executable, err := filepath.EvalSymlinks(string(parts[0]))
	if err != nil {
		return posixProcessIdentity{}, err
	}
	stringsFound := make([]string, 0, len(parts))
	for _, part := range parts[1:] {
		if len(part) != 0 {
			stringsFound = append(stringsFound, string(part))
		}
	}
	if argc > len(stringsFound) {
		return posixProcessIdentity{}, fmt.Errorf("short process argument list")
	}
	start := info.Proc.P_starttime
	sid, err := unix.Getsid(pid)
	if err != nil {
		return posixProcessIdentity{}, err
	}
	return posixProcessIdentity{
		PID: pid, ParentPID: int(info.Eproc.Ppid), PGID: int(info.Eproc.Pgid), SessionID: sid, UID: int(info.Eproc.Ucred.Uid),
		StartIdentity: strconv.FormatInt(start.Sec, 10) + ":" + strconv.FormatInt(int64(start.Usec), 10),
		Executable:    executable, Arguments: stringsFound[:argc], Environment: stringsFound[argc:], EnvironmentVisible: len(stringsFound) > argc,
	}, nil
}

func inspectPOSIXGroup(pgid int) ([]posixProcessIdentity, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if errors.Is(err, unix.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]posixProcessIdentity, 0, len(processes))
	for _, process := range processes {
		identity, err := inspectPOSIXProcess(int(process.Proc.P_pid))
		if processGone(err) {
			continue
		}
		if identityUnprovable(err) || errors.Is(err, errProcessIdentityTransient) {
			out = append(out, posixProcessIdentity{PID: int(process.Proc.P_pid), PGID: pgid, UID: -1, EnvironmentVisible: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, identity)
	}
	return out, nil
}
