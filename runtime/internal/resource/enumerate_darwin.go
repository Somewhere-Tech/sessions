//go:build darwin

package resource

import (
	"bytes"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	// Required for the //go:linkname below.
	_ "unsafe"
)

// The macOS enumerator is two calls deep and no deeper.
//
// sysctl kern.proc.all returns the whole process table -- PID, parent PID and
// executable name for every process -- in a single syscall. What it does not
// return on any modern macOS is memory or CPU: the kinfo_proc fields that once
// held them (Xrssize, P_rtime, P_uticks) have been left at zero by the kernel
// for many releases, so reading them would produce a plausible-looking zero for
// every process on the machine. That is precisely the failure this package
// exists to prevent, so those fields are not touched.
//
// Memory and CPU come from proc_pidinfo(PROC_PIDTASKINFO), which reports
// resident size in bytes and CPU in mach absolute-time units.
//
// Those units are the trap. <sys/proc_info.h> names the fields pti_total_user
// and pti_total_system with no unit in the name, and every reading of them as
// nanoseconds compiles and produces plausible small numbers. They are not
// nanoseconds on Apple Silicon: the timebase there is 24 MHz, so a tick is
// 125/3 = 41.667ns, and CPU read as nanoseconds comes out about forty-two
// times too low. That was caught here by diffing this package's output against
// `ps -o time` for the same PIDs -- 0:14.05 of runner CPU was being reported as
// 337ms -- and the ratio was 41.677, 41.668 and 41.665 across three processes.
// Intel Macs have a 1 GHz timebase where the bug is invisible, which is exactly
// why it survives in so much code. hw.tbfrequency is the ticks-per-second the
// conversion needs and is correct on both.
//
// golang.org/x/sys/unix does not wrap proc_pidinfo, so it is reached the same
// way x/sys reaches every other libSystem entry point: a dynamic import and an
// assembly trampoline (enumerate_darwin.s), with no cgo. Sessions ships pure-Go
// binaries and adding cgo to reach one function would be a far larger change
// than fourteen lines of assembly.
//
// Measured on the author's machine with 600 processes: 0.5ms for the sysctl,
// 1.9ms for 600 proc_pidinfo calls. That is the cost of a whole sampling pass
// no matter how many sessions exist.

//go:linkname syscallSyscall6 syscall.syscall6
func syscallSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:cgo_import_dynamic libc_proc_pidinfo proc_pidinfo "/usr/lib/libSystem.B.dylib"

func libcProcPidinfoTrampoline()

var libcProcPidinfoTrampolineAddr uintptr

// procPIDTaskInfo is PROC_PIDTASKINFO, the flavor that carries task-wide
// totals rather than per-thread ones.
const procPIDTaskInfo = 4

// procTaskInfo mirrors struct proc_taskinfo from <sys/proc_info.h>. The layout
// is fixed public ABI; the size is asserted against what the kernel writes
// back on every call, so a mismatch fails closed rather than reading garbage.
type procTaskInfo struct {
	VirtualSize   uint64
	ResidentSize  uint64
	TotalUser     uint64 // mach absolute-time ticks, NOT nanoseconds
	TotalSystem   uint64 // mach absolute-time ticks, NOT nanoseconds
	ThreadsUser   uint64
	ThreadsSystem uint64
	Policy        int32
	Faults        int32
	Pageins       int32
	CowFaults     int32
	MessagesSent  int32
	MessagesRecv  int32
	SyscallsMach  int32
	SyscallsUnix  int32
	Csw           int32
	Threadnum     int32
	Numrunning    int32
	Priority      int32
}

type darwinEnumerator struct{}

// SystemEnumerator returns the enumerator for this platform.
func SystemEnumerator() Enumerator { return darwinEnumerator{} }

func (darwinEnumerator) Enumerate() ([]Process, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	processes := make([]Process, 0, len(entries))
	for index := range entries {
		entry := &entries[index]
		pid := int(entry.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		process := Process{
			PID:  pid,
			PPID: int(entry.Eproc.Ppid),
			Name: commString(entry.Proc.P_comm[:]),
		}
		// A process this user may not inspect yields nothing here. It is kept
		// in the table anyway: its parent link is what lets a readable child
		// be attributed to the right session, and its cost stays absent rather
		// than being recorded as zero.
		if task, ok := taskInfo(pid); ok {
			process.RSSBytes = task.ResidentSize
			process.CPUTime = machTicksToDuration(task.TotalUser + task.TotalSystem)
		}
		processes = append(processes, process)
	}
	return processes, nil
}

// machTimebase is the ticks-per-second the kernel counts task CPU in, read
// once. A machine's timebase does not change while it is running.
var machTimebase = sync.OnceValue(func() uint64 {
	frequency, err := unix.SysctlUint32("hw.tbfrequency")
	if err != nil || frequency == 0 {
		// A macOS that will not answer for its own timebase is not a machine
		// this can guess for: assuming a value would silently scale every CPU
		// figure on the box. Zero disables the conversion and every CPU reading
		// becomes unknown, which readers render as "-".
		return 0
	}
	return uint64(frequency)
})

// machTicksToDuration converts task CPU ticks to wall duration.
//
// The division is split rather than written as ticks*1e9/frequency because
// that product overflows int64 for any long-lived process: a provider with two
// hours of CPU is 1.7e11 ticks, and multiplying by 1e9 first is 1.7e20 against
// a 9.2e18 ceiling. Seconds and remainder are converted separately so nothing
// larger than 2.4e16 is ever formed.
func machTicksToDuration(ticks uint64) time.Duration {
	frequency := machTimebase()
	if frequency == 0 {
		return 0
	}
	seconds := ticks / frequency
	remainder := ticks % frequency
	return time.Duration(seconds)*time.Second + time.Duration(remainder*uint64(time.Second)/frequency)
}

func commString(raw []byte) string {
	if index := bytes.IndexByte(raw, 0); index >= 0 {
		raw = raw[:index]
	}
	return string(raw)
}

func taskInfo(pid int) (procTaskInfo, bool) {
	var info procTaskInfo
	size := unsafe.Sizeof(info)
	written, _, errno := syscallSyscall6(
		libcProcPidinfoTrampolineAddr,
		uintptr(pid),
		procPIDTaskInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		uintptr(size),
		0,
	)
	if errno != 0 || written != uintptr(size) {
		return procTaskInfo{}, false
	}
	return info, true
}
