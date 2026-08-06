//go:build !windows

package watch

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processAlive reports whether a PID still names a running process. Signal 0 is
// the POSIX existence probe: it runs the permission and liveness checks without
// delivering anything.
//
// This deliberately matches the probe internal/recovery and internal/session
// use. The three must agree: if the registry check called a Claude process dead
// that recovery still considers live, Sessions would offer to reopen a
// conversation that a live process is appending to.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// processParents returns a pid -> ppid snapshot of the process table.
//
// Sessions launches Claude as a child of its runner, so a registry entry's pid
// is never a runner pid; deciding whether Sessions owns that Claude means
// walking its ancestry. One `ps` call gives the whole table, which is cheaper
// and more consistent than probing each pid: a table read mid-scan could show a
// process reparented to init and lose the link.
//
// An empty result means "unknown ancestry", which callers resolve to
// not-owned -- and not-owned is the conservative answer, because it makes
// Sessions treat the process as external and refuse to touch its conversation.
func processParents() map[int]int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	parents := make(map[int]int, 512)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil || pid <= 0 {
			continue
		}
		parents[pid] = ppid
	}
	return parents
}
