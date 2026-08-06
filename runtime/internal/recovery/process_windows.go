package recovery

import (
	"context"
	"errors"

	"golang.org/x/sys/windows"
)

// processAlive reports whether a PID still names a running process.
//
// The portable os.Process.Signal(0) probe cannot be used here: Go's Windows
// implementation returns syscall.EWINDOWS for every signal except os.Kill, so
// the probe answers "dead" for live and dead PIDs alike. Recovery uses this
// answer as one of its liveness signals, and Windows has no launchd to supply
// another, so a blanket false would leave a live Windows session with only the
// daemon's own view of itself.
//
// Every ambiguous outcome resolves to "alive", matching the session manager's
// probe. Recovery must not disagree with the component that owns the runner.
func processAlive(pid int) bool {
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
	// zero-timeout wait distinguishes the two without blocking.
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true
	}
	return event == uint32(windows.WAIT_TIMEOUT)
}

// processCommand returns the image path backing a PID. Every Sessions runner is
// the sessions-runner image whatever provider it hosts, so the image path
// carries the signal that matters for PID reuse. An empty result means
// "unknown", which callers treat as still-live.
func processCommand(_ context.Context, pid int) string {
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
