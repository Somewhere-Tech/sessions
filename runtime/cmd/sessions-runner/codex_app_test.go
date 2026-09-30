package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type fakeCodexTurnClient struct {
	steered      []string
	steerErr     error
	turnStarted  chan struct{}
	turnRelease  chan struct{}
	turnCanceled chan struct{}
}

func (f *fakeCodexTurnClient) SendUserTurn(ctx context.Context, _, _ string) (*codexapp.TurnStream, error) {
	if f.turnStarted == nil {
		return nil, errors.New("not implemented in this test")
	}
	close(f.turnStarted)
	select {
	case <-f.turnRelease:
		return nil, errors.New("test turn released")
	case <-ctx.Done():
		close(f.turnCanceled)
		return nil, ctx.Err()
	}
}

func (f *fakeCodexTurnClient) SteerTurn(_ context.Context, conversationID, text string) (string, error) {
	if f.steerErr != nil {
		return "", f.steerErr
	}
	if conversationID != "thread-1" {
		return "", errors.New("unexpected conversation")
	}
	f.steered = append(f.steered, text)
	return "turn-1", nil
}

func (*fakeCodexTurnClient) InterruptTurn(context.Context, string) error { return nil }

func newCodexTestRunner(t *testing.T) *codexAppRunner {
	t.Helper()
	paths := state.For(t.TempDir(), "codex-session")
	file, err := os.Create(paths.Structured)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	fake := &fakeCodexTurnClient{}
	return &codexAppRunner{
		cfg:            config{id: paths.ID, cmd: "codex", cwd: "/tmp"},
		paths:          paths,
		conversationID: "thread-1",
		logger:         log.New(io.Discard, "", 0),
		clients:        make(map[*client]struct{}),
		historyFile:    file,
		ctx:            context.Background(),
		turnClient:     fake,
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

func TestCodexInputDuringActiveTurnUsesProviderSteering(t *testing.T) {
	r := newCodexTestRunner(t)
	r.active = true

	r.handleInput("deploy the staging fix\r")

	if len(r.history) != 1 {
		t.Fatalf("history after steering = %d events, want the accepted message to be recorded", len(r.history))
	}
	var event map[string]any
	if err := json.Unmarshal(r.history[0], &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "user" || event["subtype"] != "user_steer" || event["queued"] != true ||
		event["source"] != codexapp.HistorySource || event["conversationId"] != "thread-1" {
		t.Fatalf("steering event = %#v", event)
	}
	message, _ := event["message"].(map[string]any)
	if message["content"] != "deploy the staging fix" || event["turnId"] != "turn-1" {
		t.Fatalf("steering event lost content or turn identity: %#v", event)
	}
	fake := r.turnClient.(*fakeCodexTurnClient)
	if len(fake.steered) != 1 || fake.steered[0] != "deploy the staging fix" {
		t.Fatalf("provider steering calls = %#v", fake.steered)
	}
	// Provider-accepted steering is durable, so a reconnect still explains
	// where the message is waiting.
	persisted, err := os.ReadFile(r.paths.Structured)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) == 0 {
		t.Fatal("steering input was not appended to the durable history file")
	}
	if r.composer.Len() != 0 {
		t.Fatalf("composer retained %q after a refused message", r.composer.String())
	}
}

func TestCodexHelloReportsCurrentTurnState(t *testing.T) {
	r := newCodexTestRunner(t)
	r.active = true
	r.retry = &structuredRetryController{}
	server, daemon := net.Pipe()
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

func TestCodexDaemonDisconnectDoesNotCancelActiveTurn(t *testing.T) {
	r := newCodexTestRunner(t)
	r.retry = newStructuredRetryController(r.startRetryTurn, r.appendStructured, r.publishRetryState)
	fake := r.turnClient.(*fakeCodexTurnClient)
	fake.turnStarted = make(chan struct{})
	fake.turnRelease = make(chan struct{})
	fake.turnCanceled = make(chan struct{})
	r.handleInput("long-running work\r")
	<-fake.turnStarted

	server, daemon := net.Pipe()
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
		t.Fatal("daemon disconnect ended the runner-owned provider turn")
	}
	select {
	case <-fake.turnCanceled:
		t.Fatal("daemon disconnect canceled the provider context")
	default:
	}
	close(fake.turnRelease)
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		active = r.active
		r.mu.Unlock()
		if !active {
			break
		}
		select {
		case <-deadline:
			t.Fatal("released test turn stayed active")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCodexRejectedSteeringIsExplicit(t *testing.T) {
	r := newCodexTestRunner(t)
	r.active = true
	r.turnClient.(*fakeCodexTurnClient).steerErr = errors.New("active turn is no longer steerable")

	r.handleInput("follow up\r")

	if len(r.history) != 1 {
		t.Fatalf("history after rejected steering = %d events", len(r.history))
	}
	var event map[string]any
	if err := json.Unmarshal(r.history[0], &event); err != nil {
		t.Fatal(err)
	}
	message, _ := event["error"].(string)
	if event["subtype"] != "input_rejected" || message == "" {
		t.Fatalf("rejected steering event = %#v", event)
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

func TestCodexTurnFailureAppendsClassifiedFaultBeforeLifecycleClose(t *testing.T) {
	r := newCodexTestRunner(t)
	r.recordTurnFailure("keep this prompt", 0, errors.New("unexpected status 503 Service Unavailable: The server is currently overloaded."))
	if len(r.history) != 2 {
		t.Fatalf("failure history = %d events, want fault and turn completion", len(r.history))
	}
	joined := string(r.history[0]) + "\n" + string(r.history[1])
	if !strings.Contains(string(r.history[0]), `"subtype":"provider_fault"`) ||
		!strings.Contains(string(r.history[0]), `"kind":"provider-unavailable"`) ||
		!strings.Contains(joined, "Codex API unavailable (503, overloaded)") ||
		!strings.Contains(string(r.history[1]), `"subtype":"turn_completed"`) {
		t.Fatalf("failure history = %s", joined)
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

func codexHistorySubtype(r *codexAppRunner, subtype string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, raw := range r.history {
		var value map[string]any
		if json.Unmarshal(raw, &value) == nil && value["subtype"] == subtype {
			return value, true
		}
	}
	return nil, false
}

func TestCodexRunnerHoldsAnApprovalUntilTheDaemonAnswers(t *testing.T) {
	r := newCodexTestRunner(t)
	decided := make(chan codexapp.ApprovalDecision, 1)
	go func() {
		decided <- r.awaitApproval(context.Background(), codexapp.ApprovalRequest{
			Kind: codexapp.ApprovalCommand, ConversationID: "thread-1", TurnID: "turn-1", Command: "npm test",
		})
	}()
	var requested map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if value, ok := codexHistorySubtype(r, "approval_requested"); ok {
			requested = value
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if requested == nil {
		t.Fatal("runner never announced the approval")
	}
	approval := requested["approval"].(map[string]any)
	id, _ := approval["id"].(string)
	if id == "" || approval["summary"] != "Run `npm test`" {
		t.Fatalf("announced approval = %#v", approval)
	}
	select {
	case decision := <-decided:
		t.Fatalf("runner decided %q without an answer", decision)
	case <-time.After(100 * time.Millisecond):
	}
	if err := r.resolveApproval(proto.ApprovalControl{ID: "not-waiting", Decision: proto.ApprovalAllow}); err == nil {
		t.Fatal("an unknown approval id was accepted")
	}
	payload, err := proto.EncodeApprovalControl(proto.ApprovalControl{ID: id, Decision: proto.ApprovalAllow, By: "manager-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.handleFrame(nil, proto.Frame{Type: proto.Approve, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	select {
	case decision := <-decided:
		if decision != codexapp.ApprovalAllow {
			t.Fatalf("decision = %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the waiting approval")
	}
	resolved, ok := codexHistorySubtype(r, "approval_resolved")
	if !ok {
		t.Fatal("no approval_resolved event recorded")
	}
	if outcome := resolved["approval"].(map[string]any); outcome["id"] != id || outcome["decision"] != "allow" || outcome["by"] != "manager-1" {
		t.Fatalf("resolved event = %#v", outcome)
	}

	// A request nobody answers before the turn is cancelled is denied.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		decided <- r.awaitApproval(ctx, codexapp.ApprovalRequest{Kind: codexapp.ApprovalFileChange, ConversationID: "thread-1"})
	}()
	cancel()
	select {
	case decision := <-decided:
		if decision != codexapp.ApprovalDeny {
			t.Fatalf("cancelled decision = %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled approval never returned")
	}
}
