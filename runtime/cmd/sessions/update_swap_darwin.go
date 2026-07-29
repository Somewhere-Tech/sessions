//go:build darwin

package main

import "golang.org/x/sys/unix"

func atomicSwapApps(current, next string) error {
	return unix.RenamexNp(current, next, unix.RENAME_SWAP)
}
