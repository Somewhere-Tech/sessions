//go:build windows

package resource

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows enumerator is a toolhelp snapshot plus two queries per process.
//
// CreateToolhelp32Snapshot returns every process with its parent PID and image
// name in one pass, which is the parent map the tree walk needs.
// GetProcessMemoryInfo gives WorkingSetSize -- the counter Task Manager labels
// "Memory (active private working set)" and the closest Windows equivalent of
// resident set size -- and GetProcessTimes gives cumulative kernel and user
// time as FILETIMEs, in 100-nanosecond units.
//
// GetProcessMemoryInfo is not wrapped by golang.org/x/sys/windows, so psapi is
// loaded lazily here. It is a system DLL loaded by absolute path through
// NewLazySystemDLL, never by search order.
//
// A process this token cannot open -- anything running as another user or as
// SYSTEM -- yields no memory or CPU. It stays in the table for its parent
// link and its cost is reported as absent, never as zero.

var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type windowsEnumerator struct{}

// SystemEnumerator returns the enumerator for this platform.
func SystemEnumerator() Enumerator { return windowsEnumerator{} }

func (windowsEnumerator) Enumerate() ([]Process, error) {
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
	processes := make([]Process, 0, 256)
	for {
		pid := int(entry.ProcessID)
		if pid > 0 {
			process := Process{
				PID:  pid,
				PPID: int(entry.ParentProcessID),
				Name: windows.UTF16ToString(entry.ExeFile[:]),
			}
			if rss, cpu, ok := processCost(entry.ProcessID); ok {
				process.RSSBytes = rss
				process.CPUTime = cpu
			}
			processes = append(processes, process)
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return processes, nil
}

func processCost(pid uint32) (uint64, time.Duration, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(handle)

	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	result, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if result == 0 {
		return 0, 0, false
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, 0, false
	}
	// FILETIME is a 100-nanosecond count. Kernel and user times are durations
	// here, not wall-clock instants, so Filetime.Nanoseconds -- which subtracts
	// the 1601 epoch -- must not be used on them.
	ticks := filetimeTicks(kernel) + filetimeTicks(user)
	return uint64(counters.WorkingSetSize), time.Duration(ticks) * 100 * time.Nanosecond, true
}

func filetimeTicks(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}
