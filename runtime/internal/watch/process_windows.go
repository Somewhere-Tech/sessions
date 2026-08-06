package watch

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processAlive reports whether a PID still names a running process.
//
// The portable os.Process.Signal(0) probe cannot be used here: Go's Windows
// implementation returns syscall.EWINDOWS for every signal except os.Kill, so
// it answers "dead" for live and dead PIDs alike. This mirrors the probe in
// internal/recovery, including its bias: every ambiguous outcome resolves to
// "alive", because a false "dead" here lets Sessions append to a conversation a
// live Claude process still owns.
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

// processParents returns a pid -> ppid snapshot from a Toolhelp process
// snapshot. See the Unix implementation for why the whole table is taken at
// once and why an empty result is the conservative answer.
func processParents() map[int]int {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil
	}
	parents := make(map[int]int, 512)
	for {
		if entry.ProcessID > 0 {
			parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return parents
}
