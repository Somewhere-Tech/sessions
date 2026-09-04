//go:build !windows

package fleetaccount

import "os"

func replacePrivateFile(source, destination string) error {
	return os.Rename(source, destination)
}
