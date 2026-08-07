//go:build linux

package secretstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func inspectPOSIXProcess(pid int) (posixProcessIdentity, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	fields, err := linuxProcStatFields(pid)
	if err != nil {
		return posixProcessIdentity{}, err
	}
	if fields[0] == "Z" {
		return posixProcessIdentity{}, os.ErrNotExist
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return posixProcessIdentity{}, fmt.Errorf("invalid process group id: %w", err)
	}
	uid, err := linuxProcessUID(filepath.Join(base, "status"))
	if err != nil {
		return posixProcessIdentity{}, err
	}
	executable, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return posixProcessIdentity{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return posixProcessIdentity{}, err
	}
	arguments, err := readNULStrings(filepath.Join(base, "cmdline"))
	if err != nil {
		return posixProcessIdentity{}, err
	}
	environment, err := readNULStrings(filepath.Join(base, "environ"))
	if err != nil {
		return posixProcessIdentity{}, err
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return posixProcessIdentity{}, fmt.Errorf("invalid parent pid: %w", err)
	}
	sessionID, err := strconv.Atoi(fields[3])
	if err != nil {
		return posixProcessIdentity{}, fmt.Errorf("invalid session id: %w", err)
	}
	return posixProcessIdentity{
		PID: pid, ParentPID: parentPID, PGID: pgid, SessionID: sessionID, UID: uid,
		StartIdentity: fields[19], Executable: executable, Arguments: arguments,
		Environment: environment, EnvironmentVisible: true,
	}, nil
}

func inspectPOSIXGroup(pgid int) ([]posixProcessIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []posixProcessIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fields, err := linuxProcStatFields(pid)
		if processGone(err) || identityUnprovable(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		memberPGID, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("invalid process group id: %w", err)
		}
		if memberPGID != pgid {
			continue
		}
		identity, err := inspectPOSIXProcess(pid)
		if processGone(err) {
			continue
		}
		if identityUnprovable(err) {
			sessionID, _ := strconv.Atoi(fields[3])
			out = append(out, posixProcessIdentity{PID: pid, PGID: memberPGID, SessionID: sessionID, UID: -1, EnvironmentVisible: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		if identity.PGID == pgid {
			out = append(out, identity)
		}
	}
	return out, nil
}

func linuxProcStatFields(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 {
		return nil, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) < 20 {
		return nil, fmt.Errorf("short proc stat")
	}
	return fields, nil
}

func linuxProcessUID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				return strconv.Atoi(fields[2])
			}
		}
	}
	return 0, fmt.Errorf("process uid unavailable")
}

func readNULStrings(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			out = append(out, string(part))
		}
	}
	return out, nil
}
