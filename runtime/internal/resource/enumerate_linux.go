//go:build linux

package resource

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// The Linux enumerator reads /proc directly.
//
// /proc/<pid>/stat carries the parent PID and the cumulative user and system
// times in one line; /proc/<pid>/statm carries the resident page count in a
// second, shorter one. Two small reads per process, no exec, no parsing of a
// human-formatted tool's output.
//
// Two parsing hazards are handled explicitly rather than hoped away. The comm
// field in stat is wrapped in parentheses and may itself contain spaces and
// parentheses, so fields are counted from the last ')' rather than by
// splitting the whole line. And the times are in clock ticks, not seconds:
// USER_HZ is fixed at 100 by the kernel's userspace ABI on every architecture
// Go supports, which is why it is a constant here instead of a sysconf call
// that pure Go cannot make.

const userHZ = 100

type linuxEnumerator struct{ root string }

// SystemEnumerator returns the enumerator for this platform.
func SystemEnumerator() Enumerator { return linuxEnumerator{root: "/proc"} }

func (e linuxEnumerator) Enumerate() ([]Process, error) {
	entries, err := os.ReadDir(e.root)
	if err != nil {
		return nil, err
	}
	pageSize := uint64(os.Getpagesize())
	processes := make([]Process, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		process, ok := e.read(pid, pageSize)
		if !ok {
			// The process exited between the readdir and the read, or belongs
			// to a user this daemon may not inspect. Either way it contributes
			// nothing rather than a zero.
			continue
		}
		processes = append(processes, process)
	}
	return processes, nil
}

func (e linuxEnumerator) read(pid int, pageSize uint64) (Process, bool) {
	base := e.root + "/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(base + "/stat")
	if err != nil {
		return Process{}, false
	}
	process, ok := parseProcStat(string(stat))
	if !ok {
		return Process{}, false
	}
	// statm's second field is resident pages. It is preferred over stat's
	// rss field for the same reason `ps` prefers it: identical value, and this
	// file has no comm field to parse around.
	if statm, err := os.ReadFile(base + "/statm"); err == nil {
		fields := strings.Fields(string(statm))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				process.RSSBytes = pages * pageSize
			}
		}
	}
	return process, true
}

// parseProcStat extracts PID, name, parent PID and cumulative CPU from one
// /proc/<pid>/stat line. Exported to the package's tests through the fixture
// in resource_linux_test.go rather than through a live /proc read.
func parseProcStat(line string) (Process, bool) {
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close < open {
		return Process{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return Process{}, false
	}
	// Fields after the comm field, numbered from the kernel's field 3 (state).
	rest := strings.Fields(line[close+1:])
	// state, ppid, pgrp, session, tty_nr, tpgid, flags, minflt, cminflt,
	// majflt, cmajflt, utime, stime -> utime is index 11, stime index 12.
	if len(rest) < 13 {
		return Process{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return Process{}, false
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return Process{}, false
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return Process{}, false
	}
	ticks := utime + stime
	return Process{
		PID:     pid,
		PPID:    ppid,
		Name:    line[open+1 : close],
		CPUTime: time.Duration(ticks) * time.Second / userHZ,
	}, true
}
