//go:build !windows

package background

import (
	"syscall"
	"time"
)

// processCPU is this process's CPU time, user plus system.
func processCPU() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return timevalDuration(usage.Utime) + timevalDuration(usage.Stime), true
}

func timevalDuration(value syscall.Timeval) time.Duration {
	return time.Duration(value.Sec)*time.Second + time.Duration(value.Usec)*time.Microsecond
}
