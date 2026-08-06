package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func newCodexTestRunner(t *testing.T) *codexAppRunner {
	t.Helper()
	paths := state.For(t.TempDir(), "codex-session")
	file, err := os.Create(paths.Structured)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return &codexAppRunner{
		cfg:            config{id: paths.ID, cmd: "codex", cwd: "/tmp"},
		paths:          paths,
		conversationID: "thread-1",
		logger:         log.New(io.Discard, "", 0),
		clients:        make(map[*client]struct{}),
		historyFile:    file,
	}
}

func TestCodexConversationOptionsPreservePermissionChoice(t *testing.T) {
	safe := codexConversationOptions(config{
		cwd:  "/tmp",
		args: []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"},
	})
	if safe.Sandbox != codexapp.SandboxWorkspaceWrite || safe.ApprovalPolicy != codexapp.ApprovalOnRequest {
		t.Fatalf("safe permissions = %#v", safe)
	}

	full := codexConversationOptions(config{
		cwd:  "/tmp",
		args: []string{"--dangerously-bypass-approvals-and-sandbox"},
	})
	if full.Sandbox != codexapp.SandboxDangerFullAccess || full.ApprovalPolicy != codexapp.ApprovalNever {
		t.Fatalf("full permissions = %#v", full)
	}
}

func TestCodexInterruptInputDoesNotConfuseBracketedPaste(t *testing.T) {
	for _, value := range []string{"\x1b", "\x03"} {
		if !isCodexInterruptInput(value) {
			t.Fatalf("%q was not recognized as an interrupt", value)
		}
	}
	for _, value := range []string{
		"",
		"hello",
		"\r",
		"\x1b[200~hello\x1b[201~",
		"\x1b[A",
	} {
		if isCodexInterruptInput(value) {
			t.Fatalf("%q was incorrectly recognized as an interrupt", value)
		}
	}
}

func TestCodexInputDuringActiveTurnIsReportedNotDiscarded(t *testing.T) {
	r := newCodexTestRunner(t)
	r.active = true

	r.handleInput("deploy the staging fix\r")

	if len(r.history) != 1 {
		t.Fatalf("history after a refused message = %d events, want the refusal to be recorded", len(r.history))
	}
	var event map[string]any
	if err := json.Unmarshal(r.history[0], &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "system" || event["subtype"] != "input_rejected" ||
		event["source"] != codexapp.HistorySource || event["conversationId"] != "thread-1" {
		t.Fatalf("refusal event = %#v", event)
	}
	message, _ := event["error"].(string)
	if message == "" {
		t.Fatalf("refusal carried no explanation: %#v", event)
	}
	for _, want := range []string{"Codex is still working", "not sent or queued", "send it again"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal message %q does not explain %q", message, want)
		}
	}
	// The refusal is also durable, so a client that reconnects after the turn
	// still learns the message never went anywhere.
	persisted, err := os.ReadFile(r.paths.Structured)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) == 0 {
		t.Fatal("refusal was not appended to the durable history file")
	}
	if r.composer.Len() != 0 {
		t.Fatalf("composer retained %q after a refused message", r.composer.String())
	}
}

func TestCodexBlankInputDuringActiveTurnStaysSilent(t *testing.T) {
	r := newCodexTestRunner(t)
	r.active = true

	r.handleInput("\r")

	if len(r.history) != 0 {
		t.Fatalf("blank input produced %d events, want none", len(r.history))
	}
}

func TestCodexMetadataWritePreservesDaemonOwnedFields(t *testing.T) {
	r := newCodexTestRunner(t)
	setAsideAt := int64(1717171717000)
	parent := "parent-session"
	if err := state.WriteMetadata(r.paths.Meta, state.Metadata{
		ID: r.cfg.id, Cmd: "codex", Cwd: "/tmp",
		Tags:                   map[string]string{"project": "sessions"},
		DisplayParentSessionID: &parent,
		SetAsideAt:             &setAsideAt,
		DelegationKind:         "agent",
		Permissions:            "plan",
		Lifecycle:              "task",
	}); err != nil {
		t.Fatal(err)
	}

	// A model change rewrites the runner-owned document mid-session.
	r.cfg.args = []string{"--model", "gpt-5-codex"}
	if err := r.writeMetadata(); err != nil {
		t.Fatal(err)
	}

	metadata, err := state.ReadRunnerMetadata(r.paths.Meta)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Tags["project"] != "sessions" {
		t.Fatalf("tags after a runner metadata write = %#v", metadata.Tags)
	}
	if metadata.SetAsideAt == nil || *metadata.SetAsideAt != setAsideAt {
		t.Fatalf("set-aside after a runner metadata write = %#v", metadata.SetAsideAt)
	}
	if metadata.DisplayParentSessionID == nil || *metadata.DisplayParentSessionID != parent {
		t.Fatalf("display parent after a runner metadata write = %#v", metadata.DisplayParentSessionID)
	}
	if metadata.DelegationKind != "agent" || metadata.Permissions != "plan" || metadata.Lifecycle != "task" {
		t.Fatalf("delegation/permission/lifecycle lost: %#v", metadata)
	}
	if metadata.Info.ConversationID != "thread-1" || len(metadata.Info.Args) != 2 {
		t.Fatalf("runner-owned fields were not written: %#v", metadata.Info)
	}
}

func TestRunnerMetadataWriteWithoutExistingDocumentSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-session.json")
	if err := state.WriteRunnerMetadata(path, state.Metadata{ID: "new-session", Cmd: "bash", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	metadata, err := state.ReadRunnerMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Info.ID != "new-session" || len(metadata.Tags) != 0 {
		t.Fatalf("first metadata write = %#v", metadata)
	}
}
