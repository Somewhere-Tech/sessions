//go:build windows

package state

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Windows discovery has no socket artifact to key off, so it enumerates
// metadata names. Counting every "*.json" made each session that had written a
// continuation sidecar produce a second, phantom runner id which could never be
// joined — a permanent error that buries the real lost-session signal.
func TestRunnerArtifactIDsIgnoresSidecars(t *testing.T) {
	directory := t.TempDir()
	id := "2f577cd7-565b-4861-8ea2-c77c39a20e24"
	paths := For(directory, id)
	for _, path := range []string{paths.Meta, paths.Manifest, paths.Continuation, paths.Events, paths.Log} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(directory, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	ids, err := RunnerArtifactIDs(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{id}) {
		t.Fatalf("RunnerArtifactIDs() = %#v, want exactly %q", ids, id)
	}
}

func TestRunnerArtifactIDsToleratesMissingDirectory(t *testing.T) {
	ids, err := RunnerArtifactIDs(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(ids) != 0 {
		t.Fatalf("RunnerArtifactIDs(absent) = %#v, %v", ids, err)
	}
}
