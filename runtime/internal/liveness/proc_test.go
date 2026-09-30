package liveness

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A minimal container has no ps. The daemon logged "process snapshot
// unavailable" on debian:bookworm-slim and fell back to per-runner probes that
// needed the same missing binary, so every liveness answer was "unknown".
func TestProcSnapshotReadsCommandLinesWithoutPs(t *testing.T) {
	root := t.TempDir()
	writeProcess(t, root, 41, "sessions-runner\x00--id\x00abc\x00", "")
	writeProcess(t, root, 42, "", "kworker/0:1")
	// Not a process: /proc holds plenty of names that are not pids.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o700); err != nil {
		t.Fatal(err)
	}

	processes, err := procSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := processes[41]; got != "sessions-runner --id abc" {
		t.Fatalf("pid 41 = %q, want the argument vector ps -o args= would print", got)
	}
	// A kernel thread has no cmdline. ps brackets its comm; so does this, so
	// nothing reads it as an executable path.
	if got := processes[42]; got != "[kworker/0:1]" {
		t.Fatalf("pid 42 = %q, want the bracketed comm", got)
	}
	if len(processes) != 2 {
		t.Fatalf("snapshot = %#v, want exactly the two processes", processes)
	}
}

func TestProcCommandIsEmptyForAProcessThatIsGone(t *testing.T) {
	root := t.TempDir()
	writeProcess(t, root, 7, "sh\x00-c\x00sleep 1\x00", "")

	if got := procCommand(root, 7); got != "sh -c sleep 1" {
		t.Fatalf("procCommand(7) = %q", got)
	}
	// Empty is "unknown", which every caller resolves to still-live. It must
	// never be reported as a command, because CommandMatches would then
	// declare a live runner dead.
	if got := procCommand(root, 99); got != "" {
		t.Fatalf("procCommand(missing) = %q, want unknown", got)
	}
	if got := procCommand(root, 0); got != "" {
		t.Fatalf("procCommand(0) = %q", got)
	}
}

func writeProcess(t *testing.T, root string, pid int, cmdline, comm string) {
	t.Helper()
	directory := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if cmdline != "" {
		if err := os.WriteFile(filepath.Join(directory, "cmdline"), []byte(cmdline), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if comm != "" {
		if err := os.WriteFile(filepath.Join(directory, "comm"), []byte(comm+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
