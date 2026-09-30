package watch

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// procParents reads pid -> ppid from /proc, for hosts where `ps` may not
// exist. A minimal container has no ps, and an empty ancestry map there would
// make Sessions treat every provider process as external.
//
// The root is a parameter so the parsing is testable on a host with no /proc.
func procParents(root string) map[int]int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	parents := make(map[int]int, len(entries))
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid <= 0 {
			continue
		}
		if ppid, ok := procParent(root, pid); ok {
			parents[pid] = ppid
		}
	}
	return parents
}

// procParent reads field 4 of /proc/<pid>/stat. The command name in field 2 is
// parenthesised and may itself contain spaces and parentheses, so the fields
// are counted from the last ')' rather than split from the start.
func procParent(root string, pid int) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	close := bytes.LastIndexByte(raw, ')')
	if close < 0 || close+2 >= len(raw) {
		return 0, false
	}
	fields := bytes.Fields(raw[close+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}
