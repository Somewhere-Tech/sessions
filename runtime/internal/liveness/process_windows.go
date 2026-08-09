package liveness

import (
	"context"
	"errors"

	"golang.org/x/sys/windows"
)

// ProcessAlive reports whether a PID still names a running process.
//
// The portable os.Process.Signal(0) probe cannot be used here: Go's Windows
// implementation returns syscall.EWINDOWS for every signal except os.Kill, so
// the probe answers "dead" for live and dead PIDs alike. A blanket false means
// every live Windows session is declared lost and its artifacts reaped. Ask
// the kernel directly instead.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		// The PID exists but belongs to a process this token may not open.
		// That is not evidence of death.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true
		}
		return false
	}
	defer windows.CloseHandle(handle)

	// A process object stays unsignalled until the process exits, so a
	// zero-timeout wait distinguishes the two without blocking. This is
	// preferred over GetExitCodeProcess, whose STILL_ACTIVE sentinel is
	// indistinguishable from a real exit code of 259.
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true
	}
	return event == uint32(windows.WAIT_TIMEOUT)
}

// ProcessCommand returns the image path backing a PID.
//
// Windows exposes the image path cheaply; the full command line lives in the
// target process's PEB and is not worth reading out of another process here.
// Every Sessions runner is the sessions-runner image regardless of what it
// hosts, so the image path carries the signal that matters for PID reuse. An
// empty result means "unknown", which callers treat as still-live.
func ProcessCommand(_ context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)

	buffer := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buffer[:size])
}
