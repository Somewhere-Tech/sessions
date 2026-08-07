package state

import (
	"path/filepath"
	"testing"
)

// Pinning a session is a daemon-owned edit, exactly like tagging or renaming
// it. A runner rebuilds its metadata document from the launch configuration it
// was started with and has no field for the pin, so without the merge line
// every ordinary runner write -- a model change, a conversation id arriving,
// the runner restarting -- drops it.
//
// The loss is invisible while the daemon is up, because the daemon keeps the
// pin in memory. It appears at the next restart, and what it appears as is
// worse than a forgotten preference: the session the user had exempted from
// automatic termination is eligible for it again, and nothing said so.
func TestARunnerWriteKeepsTheSessionPinnedByTheUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "66666666-7777-4888-8999-aaaaaaaaaaaa.json")

	// What the daemon wrote when it launched the runner.
	if err := WriteMetadata(path, Metadata{ID: "66666666-7777-4888-8999-aaaaaaaaaaaa", Name: "bolo"}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	// What the user then did.
	pinned := readMetadataForMerge(path)
	pinned.Pinned = true
	if err := WriteMetadata(path, pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// What the runner writes next, rebuilt from its launch configuration and
	// therefore carrying no pin at all.
	fromRunner := Metadata{ID: "66666666-7777-4888-8999-aaaaaaaaaaaa", Name: "bolo", PID: 9182}
	if err := WriteRunnerMetadata(path, fromRunner); err != nil {
		t.Fatalf("runner write: %v", err)
	}

	final := readMetadataForMerge(path)
	if !final.Pinned {
		t.Errorf("pinned = false, want it preserved — an ordinary runner write discarded " +
			"the pin, so after the next daemon restart the session the user marked as a " +
			"workbench is back to being an automatic-termination candidate with no notice")
	}
	if final.PID != 9182 {
		t.Errorf("pid = %d, want the runner's own field to still win at 9182", final.PID)
	}
}

// Unpinning has to survive the same write, and it is the direction a
// preserve-only-when-set rule would get wrong: false is a real value the user
// chose, not an absent one to fall back from.
func TestARunnerWriteKeepsASessionUnpinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "66666666-7777-4888-8999-bbbbbbbbbbbb.json")

	if err := WriteMetadata(path, Metadata{ID: "66666666-7777-4888-8999-bbbbbbbbbbbb", Pinned: true}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	unpinned := readMetadataForMerge(path)
	unpinned.Pinned = false
	if err := WriteMetadata(path, unpinned); err != nil {
		t.Fatalf("unpin: %v", err)
	}

	// A runner document carrying Pinned:true would be a stale launch-time view;
	// the daemon is the only writer, so the disk still wins.
	fromRunner := Metadata{ID: "66666666-7777-4888-8999-bbbbbbbbbbbb", Pinned: true, PID: 55}
	if err := WriteRunnerMetadata(path, fromRunner); err != nil {
		t.Fatalf("runner write: %v", err)
	}

	if readMetadataForMerge(path).Pinned {
		t.Error("pinned = true, want the unpin the user asked for to hold")
	}
}
