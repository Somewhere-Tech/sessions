//go:build !darwin

package main

import "errors"

func atomicSwapApps(_, _ string) error {
	return errors.New("atomic Sessions.app exchange requires macOS")
}
