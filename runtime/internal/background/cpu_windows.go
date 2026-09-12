//go:build windows

package background

import (
	"syscall"
	"time"
)

// processCPU is this process's CPU time, kernel plus user, as Windows accounts
// for it. A failure reads as "cannot say" rather than as zero work.
func processCPU() (time.Duration, bool) {
	var creation, exit, kernel, user syscall.Filetime
	handle, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return time.Duration(kernel.Nanoseconds()) + time.Duration(user.Nanoseconds()), true
}
