package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContinuationRoundTripIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.continuation.json")
	want := ContinuationContext{
		SchemaVersion:   ContinuationSchemaVersion,
		SourceHistoryID: "provider:claude:source", SourceProvider: "claude",
		SourceProviderID: "source", SourceTitle: "Original title", SourceCWD: "/work",
		DestinationProvider: "codex", Mode: ContinuationNativeImport,
		Messages: []ContinuationMessage{
			{Role: "user", Text: "Keep this code block:\n```go\nfmt.Println(\"ok\")\n```"},
			{Role: "assistant", Text: "Done."},
		},
	}
	if err := WriteContinuation(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("continuation permissions = %o, want 600", info.Mode().Perm())
	}
	got, err := ReadContinuation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceHistoryID != want.SourceHistoryID || len(got.Messages) != 2 ||
		got.Messages[0].Text != want.Messages[0].Text {
		t.Fatalf("continuation round trip = %+v", got)
	}
}

func TestContinuationRejectsProviderInternalRoles(t *testing.T) {
	value := ContinuationContext{
		SchemaVersion:   ContinuationSchemaVersion,
		SourceHistoryID: "source", SourceProvider: "claude", SourceCWD: "/work",
		DestinationProvider: "codex", Mode: ContinuationNativeImport,
		Messages: []ContinuationMessage{{Role: "tool", Text: "secret output"}},
	}
	if err := value.Validate(); err == nil {
		t.Fatal("expected tool-role continuation to be rejected")
	}
}
