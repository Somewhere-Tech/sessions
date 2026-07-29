//go:build !windows

package main

func runPlatformSupervisor([]string) (bool, error) {
	return false, nil
}
