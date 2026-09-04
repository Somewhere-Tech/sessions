//go:build !windows

package liveness

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessSnapshot reads every PID and command with one process-table walk.
// Discovery and listing use it instead of launching one ps process per stale
// session, which made an open app's three-second poll cost O(records*processes).
func ProcessSnapshot(ctx context.Context) (map[int]string, error) {
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcessSnapshot(string(output)), nil
}

func parseProcessSnapshot(output string) map[int]string {
	processes := make(map[int]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		separator := strings.IndexAny(line, " \t")
		if separator < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:separator])
		if err == nil && pid > 0 {
			processes[pid] = strings.TrimSpace(line[separator:])
		}
	}
	return processes
}
