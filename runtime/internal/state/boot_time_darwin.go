//go:build darwin

package state

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// readBootTime asks the kernel when this machine started. BootTime caches it.
//
// It answers the question a lost session cannot answer for itself: a runner
// whose process began before the machine booted did not crash and was not
// ended — the machine went down under it. Seven of the owner's sessions were
// retired that way on 11 September and every one of them was shown as "lost"
// with no reason.
//
// Absent rather than guessed: a platform that cannot answer returns false, and
// the caller says "daemon lost contact", which is what it actually knows.
func readBootTime() (time.Time, bool) {
	output, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return time.Time{}, false
	}
	match := darwinBootSeconds.FindStringSubmatch(string(output))
	if len(match) != 2 {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(match[1]), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}
