package watch

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

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
