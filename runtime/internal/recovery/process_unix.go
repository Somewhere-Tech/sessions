//go:build !windows

package recovery

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether a PID still names a running process. Signal 0
// is the POSIX existence probe: it performs permission and liveness checks
// without delivering anything.
//
// This mirrors the unexported probe the session manager uses to decide whether
// an unreachable runner is merely unreachable or genuinely gone. The two must
// agree: if recovery called a runner dead that the manager still owns, it would
// offer to reopen a conversation that already has a live runtime.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// processCommand returns the recorded command line for a PID so the probe can
// tell a live runner apart from an unrelated process that reused its PID. An
// empty result means "unknown", which callers treat as still-live.
func processCommand(ctx context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	output, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
