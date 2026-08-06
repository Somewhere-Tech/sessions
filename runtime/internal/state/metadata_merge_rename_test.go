package state

import (
	"path/filepath"
	"testing"
)

// Renaming a session is a daemon-owned edit, exactly like tagging it. The
// merge that protects tags, grouping, set-aside, delegation, permissions and
// lifecycle from a runner write did not protect the name, so the next time the
// runner rewrote its metadata -- at a model change, or a conversation id
// arriving -- the name went back to whatever RUNNER_NAME was at launch.
//
// No concurrency is required. The runner rebuilds its document from the launch
// configuration it was started with (cmd/sessions-runner/main.go:440 reads
// RUNNER_NAME once), so it always carries the old name and always overwrites.
// Nothing fails visibly: the daemon keeps the new name in memory and the loss
// only appears after a restart re-reads the file.
func TestARunnerWriteKeepsTheNameTheUserChose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "11111111-2222-4333-8444-555555555555.json")

	// What the daemon wrote when it launched the runner.
	if err := WriteMetadata(path, Metadata{ID: "11111111-2222-4333-8444-555555555555", Name: "lane-3"}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	// What the user then did.
	renamed := readMetadataForMerge(path)
	renamed.Name = "billing migration"
	renamed.Description = "why this session exists"
	if err := WriteMetadata(path, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// What the runner writes next, rebuilt from its launch configuration and
	// therefore still carrying the name it was started with.
	fromRunner := Metadata{ID: "11111111-2222-4333-8444-555555555555", Name: "lane-3", PID: 4242}
	if err := WriteRunnerMetadata(path, fromRunner); err != nil {
		t.Fatalf("runner write: %v", err)
	}

	final := readMetadataForMerge(path)
	if final.Name != "billing migration" {
		t.Errorf("name = %q, want %q — the rename the user asked for was discarded by "+
			"an ordinary runner write, so the session comes back under its launch name "+
			"after the next daemon restart", final.Name, "billing migration")
	}
	if final.Description != "why this session exists" {
		t.Errorf("description = %q, want it preserved for the same reason", final.Description)
	}
	if final.PID != 4242 {
		t.Errorf("pid = %d, want the runner's own field to still win at 4242", final.PID)
	}
}
