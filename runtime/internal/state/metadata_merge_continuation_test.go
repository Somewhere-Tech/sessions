package state

import (
	"path/filepath"
	"testing"
)

// The daemon writes continuation lineage into metadata at creation
// (Registry.Create), but the PTY runner rebuilds its metadata document purely
// from launch configuration and has no way to express those fields. Without
// preserving them, opening a Claude continuation in the default interactive
// runtime lost its link back to the source conversation the moment the runner
// wrote metadata.
func TestRunnerMetadataWritePreservesContinuationLineage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	daemonWritten := Metadata{
		ID: "abc", Cmd: "claude", Cwd: "/tmp",
		ContinuedFromHistoryID: "provider-history:claude:1234",
		ContinuedFromProvider:  "claude",
		ContinuationMode:       "linked",
		ImportedMessageCount:   42,
		Lifecycle:              "task",
		Tags:                   map[string]string{"team": "core"},
	}
	if err := WriteMetadata(path, daemonWritten); err != nil {
		t.Fatal(err)
	}

	// What a PTY runner rebuilds from cfg: no continuation, no daemon fields.
	if err := WriteRunnerMetadata(path, Metadata{ID: "abc", Cmd: "claude", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}

	after := readMetadataForMerge(path)
	if after.ContinuedFromHistoryID != "provider-history:claude:1234" {
		t.Errorf("ContinuedFromHistoryID = %q, want it preserved", after.ContinuedFromHistoryID)
	}
	if after.ContinuedFromProvider != "claude" {
		t.Errorf("ContinuedFromProvider = %q, want it preserved", after.ContinuedFromProvider)
	}
	if after.ContinuationMode != "linked" {
		t.Errorf("ContinuationMode = %q, want it preserved", after.ContinuationMode)
	}
	if after.ImportedMessageCount != 42 {
		t.Errorf("ImportedMessageCount = %d, want it preserved", after.ImportedMessageCount)
	}
	if after.Lifecycle != "task" {
		t.Errorf("Lifecycle = %q, want it preserved", after.Lifecycle)
	}
}

// A structured runner does carry its own continuation values from the sidecar.
// On a first write, with no document on disk to preserve, those must survive.
func TestFirstRunnerMetadataWriteKeepsItsOwnContinuationValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := WriteRunnerMetadata(path, Metadata{
		ID: "abc", Cmd: "codex", Cwd: "/tmp",
		ContinuedFromHistoryID: "provider-history:codex:9",
		ContinuedFromProvider:  "codex",
		ContinuationMode:       "copied",
		ImportedMessageCount:   7,
	}); err != nil {
		t.Fatal(err)
	}
	after := readMetadataForMerge(path)
	if after.ContinuedFromHistoryID != "provider-history:codex:9" || after.ImportedMessageCount != 7 {
		t.Fatalf("a first write discarded the runner's own continuation values: %+v", after)
	}
}

// Provider identity is initially resolved by the daemon from the exact argv it
// launches. A plain PTY runner does not own that identity, so rebuilding its
// metadata without those fields must not sever the conversation binding.
func TestRunnerMetadataWritePreservesProviderConversationIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	const providerID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if err := WriteMetadata(path, Metadata{
		ID: "abc", Cmd: "claude", Cwd: "/tmp",
		ConversationID: providerID, ClaudeSessionID: providerID,
	}); err != nil {
		t.Fatal(err)
	}

	if err := WriteRunnerMetadata(path, Metadata{ID: "abc", Cmd: "claude", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}

	after := readMetadataForMerge(path)
	if after.ConversationID != providerID || after.ClaudeSessionID != providerID {
		t.Fatalf("runner metadata write severed provider identity: conversation %q, Claude %q", after.ConversationID, after.ClaudeSessionID)
	}
}

func TestRunnerMetadataNonEmptyProviderIdentityRemainsAuthoritative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := WriteMetadata(path, Metadata{
		ID: "abc", Cmd: "codex", Cwd: "/tmp", ConversationID: "provisional",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRunnerMetadata(path, Metadata{
		ID: "abc", Cmd: "codex", Cwd: "/tmp", ConversationID: "provider-confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	if got := readMetadataForMerge(path).ConversationID; got != "provider-confirmed" {
		t.Fatalf("ConversationID = %q, want runner-confirmed value", got)
	}
}
