//go:build linux

package liveness

import (
	"context"
	"os"
	"syscall"
)

// ProcessAlive reports whether a PID still names a running process. Signal 0
// is the POSIX existence probe: it performs the permission and liveness checks
// without delivering anything.
//
// Prefer RunnerAlive. A bare PID cannot distinguish a session's runner from
// whatever else the kernel handed that number to.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// ProcessCommand returns the command line recorded for a PID so CommandMatches
// can tell a live runner from an unrelated process that reused its PID. An
// empty result means "unknown", which callers treat as still-live.
func ProcessCommand(_ context.Context, pid int) string {
	return procCommand("/proc", pid)
}
