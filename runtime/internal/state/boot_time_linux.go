//go:build linux

package state

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// readBootTime reads /proc/stat's btime. See the darwin implementation for
// why a lost session needs it; BootTime caches the answer.
func readBootTime() (time.Time, bool) {
	return bootTimeFrom("/proc/stat")
}

func bootTimeFrom(path string) (time.Time, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0), true
	}
	return time.Time{}, false
}
