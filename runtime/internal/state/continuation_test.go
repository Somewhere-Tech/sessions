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

func TestContinuationAllowsSameProviderOnlyForFork(t *testing.T) {
	value := ContinuationContext{
		SchemaVersion:   ContinuationSchemaVersion,
		SourceHistoryID: "source", SourceProvider: "claude", SourceCWD: "/work",
		DestinationProvider: "claude", Mode: ContinuationLinkedSearch,
		Messages: []ContinuationMessage{{Role: "user", Text: "branch here"}},
	}
	if err := value.Validate(); err == nil {
		t.Fatal("expected ordinary same-provider continuation to be rejected")
	}
	value.Fork = true
	if err := value.Validate(); err != nil {
		t.Fatalf("fork validation: %v", err)
	}
}

func TestContinuationValidatesExactForkPoint(t *testing.T) {
	point := 3
	value := ContinuationContext{
		SchemaVersion:   ContinuationSchemaVersion,
		SourceHistoryID: "source", SourceProvider: "claude",
		SourceCWD: t.TempDir(), DestinationProvider: "codex",
		Mode: ContinuationNativeImport, Fork: true,
		ForkPointIndex: &point, ForkPointMessageID: "message-hash",
		Messages: []ContinuationMessage{{Role: "user", Text: "Branch here."}},
	}
	if err := value.Validate(); err != nil {
		t.Fatalf("valid exact fork point rejected: %v", err)
	}
	value.ForkPointMessageID = ""
	if err := value.Validate(); err == nil {
		t.Fatal("fork point without stable message id was accepted")
	}
}
