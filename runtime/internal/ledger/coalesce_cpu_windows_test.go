//go:build windows

package ledger

import (
	"syscall"
	"testing"
	"time"
)

func processCPUForTest(t *testing.T) time.Duration {
	t.Helper()
	handle, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	// Process times are durations in 100 ns ticks, not calendar timestamps.
	userTicks := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	kernelTicks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	return time.Duration(userTicks+kernelTicks) * 100 * time.Nanosecond
}
