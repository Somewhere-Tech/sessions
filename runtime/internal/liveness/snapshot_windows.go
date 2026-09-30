//go:build windows

package liveness

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcessSnapshot uses one Toolhelp snapshot. The executable name is enough
// for the conservative Windows identity rule, where every native session is
// hosted by sessions-runner.exe.
func ProcessSnapshot(_ context.Context) (map[int]string, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	processes := make(map[int]string)
	for {
		if entry.ProcessID > 0 {
			processes[int(entry.ProcessID)] = windows.UTF16ToString(entry.ExeFile[:])
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return processes, nil
}
