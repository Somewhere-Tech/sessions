package claudep

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizeEventAndLifecycle(t *testing.T) {
	raw := json.RawMessage(`{"type":"assistant","session_id":"session-1","message":{"role":"assistant","content":[{"type":"text","text":"OK"},{"type":"tool_use","name":"Read"}]}}`)
	event, err := NormalizeEvent(raw, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "assistant" || event.SessionID != "session-1" || event.Message != "OK" {
		t.Fatalf("normalized event = %#v", event)
	}
	var value map[string]any
	if err := json.Unmarshal(event.Raw, &value); err != nil || value["source"] != HistorySource {
		t.Fatalf("normalized raw = %s: %v", event.Raw, err)
	}
	started, _ := TurnStartedEvent("session-1", time.Unix(2, 0))
	if working, authoritative := HistoryLifecycle(started); !working || !authoritative {
		t.Fatalf("started lifecycle = %v %v", working, authoritative)
	}
	result, err := NormalizeEvent(json.RawMessage(`{"type":"result","subtype":"success","session_id":"session-1","result":"OK","usage":{"input_tokens":1,"output_tokens":1}}`), time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, authoritative := HistoryLifecycle(result.Raw); working || !authoritative {
		t.Fatalf("result lifecycle = %v %v", working, authoritative)
	}
}

func TestNormalizeEventRejectsMissingSessionID(t *testing.T) {
	if event, err := NormalizeEvent(
		json.RawMessage(`{"type":"result","subtype":"success","result":"poisoned"}`),
		time.Unix(1, 0),
	); err == nil {
		t.Fatalf("NormalizeEvent accepted result without session id: %#v", event)
	}
}

func TestInputRejectedEventIsVisibleButNotLifecycle(t *testing.T) {
	raw, err := InputRejectedEvent("session-1", "still working; not queued", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event["type"] != "system" || event["subtype"] != "input_rejected" ||
		event["error"] != "still working; not queued" {
		t.Fatalf("input rejected event = %#v", event)
	}
	if _, authoritative := HistoryLifecycle(raw); authoritative {
		t.Fatal("input rejection incorrectly closed the active turn")
	}
}

func TestTurnArgsForcePerTurnResumeAndStructuredOutput(t *testing.T) {
	first := turnArgs("hello", TurnOptions{
		SessionID: "session-1", Model: "sonnet",
		ExtraArgs: []string{"--dangerously-skip-permissions", "--session-id", "wrong", "--output-format", "text"},
	})
	wantFirst := []string{"--dangerously-skip-permissions", "-p", "--output-format", "stream-json", "--verbose", "--model", "sonnet", "--session-id", "session-1", "hello"}
	assertStrings(t, first, wantFirst)
	second := turnArgs("again", TurnOptions{SessionID: "session-1", Resume: true})
	wantSecond := []string{"-p", "--output-format", "stream-json", "--verbose", "--resume", "session-1", "again"}
	assertStrings(t, second, wantSecond)
}

func TestSubscriptionEnvironmentDropsAnthropicAPIKey(t *testing.T) {
	got := withoutAnthropicAPIKey([]string{"HOME=/tmp/home", "ANTHROPIC_API_KEY=metered", "PATH=/bin"})
	assertStrings(t, got, []string{"HOME=/tmp/home", "PATH=/bin"})
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestApprovalSummaryAndLifecycle(t *testing.T) {
	cases := map[string]string{
		"Bash":     "Run `npm test`",
		"Edit":     "Change src/a.go",
		"WebFetch": "Fetch https://example.com",
		"Grep":     "Use Grep",
	}
	input := json.RawMessage(`{"command":"npm   test","file_path":"src/a.go","url":"https://example.com"}`)
	for tool, want := range cases {
		if got := ApprovalSummary(tool, input); got != want {
			t.Fatalf("ApprovalSummary(%s) = %q, want %q", tool, got, want)
		}
	}
	requested, err := ApprovalRequestedEvent("s-1", "approval-1", "Bash", input, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, ok := HistoryLifecycle(requested); working || !ok {
		t.Fatalf("requested lifecycle = %v, %v", working, ok)
	}
	resolved, err := ApprovalResolvedEvent("s-1", "approval-1", "allow", "", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if working, ok := HistoryLifecycle(resolved); !working || !ok {
		t.Fatalf("resolved lifecycle = %v, %v", working, ok)
	}
}
