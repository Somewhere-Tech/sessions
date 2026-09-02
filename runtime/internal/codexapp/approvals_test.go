package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// captureTransport records what the client writes and never produces input.
type captureTransport struct {
	writes chan []byte
}

func (t *captureTransport) Read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (t *captureTransport) Write(_ context.Context, data []byte) error {
	t.writes <- append([]byte(nil), data...)
	return nil
}

func (t *captureTransport) Close() error { return nil }

func nextWrite(t *testing.T, transport *captureTransport) map[string]any {
	t.Helper()
	select {
	case data := <-transport.writes:
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("decode write %s: %v", data, err)
		}
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("client wrote nothing")
		return nil
	}
}

func TestApprovalRequestsRouteThroughTheHandler(t *testing.T) {
	transport := &captureTransport{writes: make(chan []byte, 8)}
	client := &Client{
		transport: transport,
		pending:   make(map[string]chan callResponse),
		turns:     make(map[string]*turnState),
		convs:     make(map[string]conversationDefaults),
	}
	command := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"npm test","cwd":"/repo"}`)

	// Without a handler the client accepts for the session: an autonomous lane.
	client.handleServerRequest(wireMessage{ID: json.RawMessage(`"1"`), Method: "item/commandExecution/requestApproval", Params: command})
	reply := nextWrite(t, transport)
	if reply["result"].(map[string]any)["decision"] != "acceptForSession" {
		t.Fatalf("default reply = %#v", reply)
	}

	seen := make(chan ApprovalRequest, 4)
	decision := ApprovalDeny
	client.HandleApprovals(func(_ context.Context, request ApprovalRequest) ApprovalDecision {
		seen <- request
		return decision
	})
	client.handleServerRequest(wireMessage{ID: json.RawMessage(`"2"`), Method: "item/commandExecution/requestApproval", Params: command})
	reply = nextWrite(t, transport)
	if reply["id"] != "2" || reply["result"].(map[string]any)["decision"] != "decline" {
		t.Fatalf("declined reply = %#v", reply)
	}
	request := <-seen
	if request.Kind != ApprovalCommand || request.Command != "npm test" || request.CWD != "/repo" ||
		request.ConversationID != "thread-1" || request.TurnID != "turn-1" || request.ItemID != "item-1" {
		t.Fatalf("parsed request = %#v", request)
	}
	if request.Summary() != "Run `npm test`" {
		t.Fatalf("summary = %q", request.Summary())
	}

	// A permission grant echoes the requested profile with the chosen scope.
	decision = ApprovalAllowForSession
	client.handleServerRequest(wireMessage{
		ID: json.RawMessage(`"3"`), Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-2","cwd":"/repo","permissions":{"network":true},"reason":"fetch deps"}`),
	})
	reply = nextWrite(t, transport)
	result := reply["result"].(map[string]any)
	if result["scope"] != "session" || result["permissions"].(map[string]any)["network"] != true {
		t.Fatalf("permission reply = %#v", reply)
	}
	if request = <-seen; request.Kind != ApprovalPermissions || !strings.Contains(request.Summary(), "fetch deps") {
		t.Fatalf("permission request = %#v", request)
	}

	// The legacy protocol carries argv and expects its own vocabulary.
	decision = ApprovalAllow
	client.handleServerRequest(wireMessage{
		ID: json.RawMessage(`"4"`), Method: "execCommandApproval",
		Params: json.RawMessage(`{"conversationId":"conv-1","callId":"call-1","command":["bash","-lc","ls"],"cwd":"/"}`),
	})
	reply = nextWrite(t, transport)
	if reply["result"].(map[string]any)["decision"] != "approved" {
		t.Fatalf("legacy reply = %#v", reply)
	}
	if request = <-seen; request.Command != "bash -lc ls" || request.ConversationID != "conv-1" || request.ItemID != "call-1" {
		t.Fatalf("legacy request = %#v", request)
	}

	// Anything else is still refused as unsupported.
	client.handleServerRequest(wireMessage{ID: json.RawMessage(`"5"`), Method: "something/else"})
	if reply = nextWrite(t, transport); reply["error"] == nil {
		t.Fatalf("unsupported request reply = %#v", reply)
	}
}

func TestApprovalEventsDriveLifecycle(t *testing.T) {
	requested, err := ApprovalRequestedEvent("approval-1", ApprovalRequest{
		Kind: ApprovalFileChange, ConversationID: "thread-1", TurnID: "turn-1", Reason: "write outside the workspace",
	}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, ok := HistoryLifecycle(requested); working || !ok {
		t.Fatalf("requested lifecycle = %v, %v", working, ok)
	}
	var value struct {
		Approval struct {
			ID, Kind, Summary string
		} `json:"approval"`
	}
	if json.Unmarshal(requested, &value) != nil || value.Approval.ID != "approval-1" || value.Approval.Kind != "file-change" ||
		value.Approval.Summary != "Change files — write outside the workspace" {
		t.Fatalf("requested event = %s", requested)
	}
	resolved, err := ApprovalResolvedEvent("thread-1", "approval-1", ApprovalAllow, "manager-1", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, ok := HistoryLifecycle(resolved); !working || !ok {
		t.Fatalf("resolved lifecycle = %v, %v", working, ok)
	}
}
