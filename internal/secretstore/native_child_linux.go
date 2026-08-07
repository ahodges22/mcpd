//go:build linux

package secretstore

import "golang.org/x/sys/unix"

func (s *POSIXSupervisor) signalNativeChild(marker helperMarker, signal unix.Signal) (bool, error) {
	return true, nil
}
