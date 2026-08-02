//go:build linux

package config

import "golang.org/x/sys/unix"

func exchangeFiles(displaced, incoming string) error {
	return unix.Renameat2(unix.AT_FDCWD, displaced, unix.AT_FDCWD, incoming, unix.RENAME_EXCHANGE)
}
