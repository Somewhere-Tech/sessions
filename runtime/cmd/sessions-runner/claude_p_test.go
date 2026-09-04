package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
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

func TestClaudeHelloReportsCurrentTurnState(t *testing.T) {
	server, daemon := net.Pipe()
	r := &claudeStructuredRunner{
		cfg:    config{id: "claude-session", cmd: "claude", cwd: "/tmp"},
		logger: log.New(io.Discard, "", 0), clients: make(map[*client]struct{}), active: true,
		retry: &structuredRetryController{},
	}
	done := make(chan struct{})
	go func() {
		r.serveClient(server)
		close(done)
	}()
	frame, err := proto.Read(daemon)
	if err != nil {
		t.Fatal(err)
	}
	var got hello
	if frame.Type != proto.Hello || json.Unmarshal(frame.Payload, &got) != nil {
		t.Fatalf("runner hello = type %v payload %s", frame.Type, frame.Payload)
	}
	if got.ProtocolVersion != proto.ProtocolVersion || got.Turn == nil || !got.Turn.Working {
		t.Fatalf("runner hello turn state = %#v", got)
	}
	_ = daemon.Close()
	<-done
}

func TestClaudeDaemonDisconnectDoesNotCancelActiveTurn(t *testing.T) {
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	server, daemon := net.Pipe()
	r := &claudeStructuredRunner{
		cfg: config{id: "claude-session", cmd: "claude", cwd: "/tmp"}, ctx: context.Background(),
		logger: log.New(io.Discard, "", 0), clients: make(map[*client]struct{}), active: true,
		turnCancel: turnCancel, retry: &structuredRetryController{},
	}
	detached := make(chan struct{})
	go func() {
		r.serveClient(server)
		close(detached)
	}()
	if _, err := proto.Read(daemon); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Close()
	<-detached
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if !active {
		t.Fatal("daemon disconnect ended the runner-owned Claude turn")
	}
	select {
	case <-turnCtx.Done():
		t.Fatal("daemon disconnect canceled the Claude process context")
	default:
	}
}

func TestClaudeResultFaultUsesAPIStatusAndRunnerPersistsStreamFailures(t *testing.T) {
	event := claudep.Event{Type: "result", Message: "API Error", Raw: json.RawMessage(
		`{"type":"result","is_error":true,"api_error_status":529,"result":"API Error: Repeated 529 Overloaded errors."}`,
	)}
	fault, _, failed := claudeResultProviderFault(event)
	if !failed || fault.Kind != "provider-unavailable" || fault.Status != 529 || fault.Detail != "Claude API overloaded (529)" {
		t.Fatalf("Claude result fault = %#v, failed=%v", fault, failed)
	}
	paths := state.For(t.TempDir(), "claude-session")
	file, err := os.Create(paths.ClaudeP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	r := &claudeStructuredRunner{
		paths: paths, sessionID: "claude-uuid", historyFile: file,
		logger: log.New(io.Discard, "", 0), clients: make(map[*client]struct{}), ctx: context.Background(),
	}
	r.recordTurnFailure("keep this prompt", 0, errors.New("connection refused"))
	if len(r.history) != 2 || !strings.Contains(string(r.history[0]), `"subtype":"provider_fault"`) ||
		!strings.Contains(string(r.history[0]), "Claude API connection failed") ||
		!strings.Contains(string(r.history[1]), `"type":"result"`) {
		t.Fatalf("Claude stream failure history = %s", r.history)
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
