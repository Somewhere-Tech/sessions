package state

import (
	"errors"
	"path/filepath"
	"testing"
)

// A pin has to be on disk before it is acknowledged. The whole point of the
// mark is that the automatic machinery honours it, and that machinery runs
// again after a daemon restart, when the only record of the user's decision is
// the metadata file.
func TestUpdatePinnedPersistsBeforeItIsAcknowledged(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	if err := EnsureDir(runnerDir); err != nil {
		t.Fatal(err)
	}
	const id = "cccccccc-dddd-4eee-8fff-000000000001"
	path := filepath.Join(runnerDir, id+".json")
	if err := WriteMetadata(path, Metadata{ID: id, Cmd: "/bin/sh", Cwd: root, Name: "bolo"}); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(Config{RunnerStateDir: runnerDir}, nil)

	pinned, err := registry.UpdatePinned(id, true)
	if err != nil || !pinned {
		t.Fatalf("UpdatePinned(true) = %v, %v", pinned, err)
	}
	stored, err := readRunnerMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Pinned {
		t.Fatal("the pin was acknowledged but not written to runner metadata, so it is " +
			"gone at the next daemon restart while the user believes the session is protected")
	}
	// The rest of the document is the runner's and must survive the edit.
	if stored.Name != "bolo" || stored.Info.Cmd != "/bin/sh" {
		t.Fatalf("pinning rewrote unrelated metadata: name=%q cmd=%q", stored.Name, stored.Info.Cmd)
	}

	unpinned, err := registry.UpdatePinned(id, false)
	if err != nil || unpinned {
		t.Fatalf("UpdatePinned(false) = %v, %v", unpinned, err)
	}
	stored, err = readRunnerMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Pinned {
		t.Fatal("unpinning did not clear the persisted pin")
	}
}

// A session the daemon has no record of is a missing session, not a bad
// request: the caller almost always has a stale id from before a restart, and
// 404 is what tells it to look the id up again.
func TestUpdatePinnedReportsAnUnknownSessionAsNotFound(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	if err := EnsureDir(runnerDir); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(Config{RunnerStateDir: runnerDir}, nil)
	for _, id := range []string{"cccccccc-dddd-4eee-8fff-000000000002", "", "..", "../escape"} {
		if _, err := registry.UpdatePinned(id, true); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("UpdatePinned(%q) error = %v, want ErrSessionNotFound", id, err)
		}
	}
}

// Discovery is the path a pin actually has to survive: the daemon restarts,
// re-reads each runner document, and rebuilds SessionInfo from it. A pin that
// is written but never read back is the same failure as one that was never
// written.
func TestPinnedMetadataSurvivesIntoSessionInfo(t *testing.T) {
	const id = "cccccccc-dddd-4eee-8fff-000000000003"
	encoded := []byte(`{"id":"` + id + `","cmd":"/bin/sh","cwd":"/tmp","pinned":true}`)
	metadata, err := parseRunnerMetadata(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Pinned {
		t.Fatal("a pinned runner document was read back unpinned, so every restart " +
			"quietly unpins every pinned session")
	}
}
