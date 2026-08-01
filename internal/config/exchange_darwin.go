//go:build darwin

package config

import "golang.org/x/sys/unix"

func exchangeFiles(displaced, incoming string) error {
	return unix.RenamexNp(displaced, incoming, unix.RENAME_SWAP)
}
