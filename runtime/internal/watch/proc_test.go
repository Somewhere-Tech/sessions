package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Sessions launches a provider as a child of its runner, so deciding whether
// Sessions owns a process means walking its ancestry. On a host without ps the
// answer has to come from /proc, or every provider reads as external.
func TestProcParentsReadsAncestryWithoutPs(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, 10, "sessions-runner", 1)
	// A command name with a space and a parenthesis: field 2 of stat is
	// parenthesised and not escaped, which is why the fields are counted from
	// the last ')' rather than split from the start.
	writeStat(t, root, 11, "claude (node)", 10)
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o700); err != nil {
		t.Fatal(err)
	}

	parents := procParents(root)
	if parents[10] != 1 {
		t.Fatalf("ppid of 10 = %d, want 1", parents[10])
	}
	if parents[11] != 10 {
		t.Fatalf("ppid of 11 = %d, want the runner that launched it", parents[11])
	}
	if len(parents) != 2 {
		t.Fatalf("parents = %#v, want exactly the two processes", parents)
	}
}

func TestProcParentsIsEmptyWhenProcIsUnreadable(t *testing.T) {
	// Unknown ancestry, which callers resolve to not-owned: Sessions then
	// treats the process as external and refuses to touch its conversation.
	if parents := procParents(filepath.Join(t.TempDir(), "missing")); len(parents) != 0 {
		t.Fatalf("parents = %#v, want none", parents)
	}
}

func writeStat(t *testing.T, root string, pid int, comm string, ppid int) {
	t.Helper()
	directory := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%d (%s) S %d 1 0 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 500\n", pid, comm, ppid)
	if err := os.WriteFile(filepath.Join(directory, "stat"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}
