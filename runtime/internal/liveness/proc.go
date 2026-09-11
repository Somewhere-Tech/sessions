package liveness

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Reading /proc instead of shelling out to ps.
//
// The snapshot and the per-pid command below used to be one `ps` invocation
// each. A minimal container has no ps: on a debian:bookworm-slim host the
// daemon logged `process snapshot unavailable; using per-runner probes: exec:
// "ps": executable file not found in $PATH` and fell back to probing every
// runner one at a time — which needs the same missing binary to answer "what
// is this pid running?", so every answer was "unknown".
//
// The root is a parameter so the parsing is testable on a host that has no
// /proc at all; Linux passes "/proc".

// procSnapshot reads every pid and its command line from one /proc walk.
func procSnapshot(root string) (map[int]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	processes := make(map[int]string, len(entries))
	for _, entry := range entries {
		pid, ok := numericName(entry.Name())
		if !ok {
			continue
		}
		if command := readProcCommand(root, pid); command != "" {
			processes[pid] = command
		}
	}
	return processes, nil
}

// procCommand is the command line recorded for one pid, or "" for a process
// that is gone or that this user may not read. Empty means "unknown", which
// every caller resolves to still-live.
func procCommand(root string, pid int) string {
	if pid <= 0 {
		return ""
	}
	return readProcCommand(root, pid)
}

// readProcCommand prefers cmdline, which is the full argument vector ps -o
// args= prints. A kernel thread has an empty cmdline; its comm is the only
// name it has, and it is bracketed the way ps brackets it so nothing reads it
// as an executable path.
func readProcCommand(root string, pid int) string {
	directory := filepath.Join(root, strconv.Itoa(pid))
	if raw, err := os.ReadFile(filepath.Join(directory, "cmdline")); err == nil {
		if command := strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(raw, "\x00"), []byte{0}, []byte{' '}))); command != "" {
			return command
		}
	}
	raw, err := os.ReadFile(filepath.Join(directory, "comm"))
	if err != nil {
		return ""
	}
	if name := strings.TrimSpace(string(raw)); name != "" {
		return "[" + name + "]"
	}
	return ""
}

func numericName(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
