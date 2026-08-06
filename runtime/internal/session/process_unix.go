//go:build !windows

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// processAlive reports whether a PID still names a running process. Signal 0
// is the POSIX existence probe: it performs permission and liveness checks
// without delivering anything.
func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// processCommand returns the recorded command line for a PID so discovery can
// tell a live runner apart from an unrelated process that reused its PID. An
// empty result means "unknown", which callers treat as still-live.
func processCommand(pid int) string {
	commandCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "ps", "-p", fmt.Sprint(pid), "-o", "args=")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
