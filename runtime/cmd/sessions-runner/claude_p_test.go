package main

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestStructuredProfileLoginHintKeepsToolErrorAndTeachesPTYLogin(t *testing.T) {
	toolErr := errors.New("Claude Code is not logged in")
	if got := structuredProfileLoginHint(toolErr, "work"); !errors.Is(got, toolErr) ||
		!strings.Contains(got.Error(), "Claude Code is not logged in") ||
		!strings.Contains(got.Error(), "new profile: open a regular PTY session with --profile work once to log in") {
		t.Fatalf("profile hint = %v", got)
	}
	if got := structuredProfileLoginHint(toolErr, ""); got != toolErr {
		t.Fatalf("default structured error changed: %v", got)
	}
}

func TestClaudeMetadataWritePreservesDaemonOwnedFields(t *testing.T) {
	paths := state.For(t.TempDir(), "claude-session")
	setAsideAt := int64(1717171717000)
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: paths.ID, Cmd: "claude", Cwd: "/tmp",
		Tags:       map[string]string{"review": "pending"},
		SetAsideAt: &setAsideAt, Lifecycle: "task", Permissions: "acceptEdits",
	}); err != nil {
		t.Fatal(err)
	}
	r := &claudeStructuredRunner{
		cfg:       config{id: paths.ID, cmd: "claude", cwd: "/tmp", args: []string{"--model", "opus"}},
		paths:     paths,
		sessionID: "claude-uuid",
		logger:    log.New(io.Discard, "", 0),
		clients:   make(map[*client]struct{}),
	}
	if err := r.writeMetadata(); err != nil {
		t.Fatal(err)
	}

	metadata, err := state.ReadRunnerMetadata(paths.Meta)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Tags["review"] != "pending" || metadata.Lifecycle != "task" ||
		metadata.Permissions != "acceptEdits" || metadata.SetAsideAt == nil || *metadata.SetAsideAt != setAsideAt {
		t.Fatalf("daemon-owned metadata after a Claude runner write = %#v", metadata)
	}
	if metadata.Info.ClaudeSessionID != "claude-uuid" {
		t.Fatalf("runner-owned metadata was not written: %#v", metadata.Info)
	}
}
