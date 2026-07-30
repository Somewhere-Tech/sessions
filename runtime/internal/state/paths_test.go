package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteMetadataAtomicallyReplacesValidPrivateFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	if err := WriteMetadata(path, Metadata{ID: "before", Cmd: "/bin/sh"}); err != nil {
		t.Fatal(err)
	}

	const writes = 200
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := 0; index < writes; index++ {
			if err := WriteMetadata(path, Metadata{ID: "after", Cmd: "/bin/sh", Cols: index}); err != nil {
				t.Errorf("WriteMetadata() = %v", err)
				return
			}
		}
	}()

	for index := 0; index < writes; index++ {
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var metadata Metadata
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			t.Fatalf("reader observed partial metadata %q: %v", encoded, err)
		}
		if metadata.ID != "before" && metadata.ID != "after" {
			t.Fatalf("reader observed unexpected metadata id %q", metadata.ID)
		}
	}
	writer.Wait()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("metadata mode = %o, want 600", got)
	}
	temporary, err := filepath.Glob(filepath.Join(root, ".session.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporary) != 0 {
		t.Fatalf("metadata temporary files remained: %v", temporary)
	}
}
