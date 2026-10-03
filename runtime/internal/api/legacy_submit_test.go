package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type legacyEchoInput struct {
	sessionService
	session *state.Session
	inputs  []string
	echo    *string
	test    *testing.T
}

func TestLegacyClaudePastedHistoryAcknowledgesOneSubmitWithoutResending(t *testing.T) {
	text := "# Start now\n" + strings.Repeat("Keep the complete instructions 🙂\n", 256)
	echo := "\n\n<pasted_content id=\"7ab0\">\n" + text + "\n</pasted_content id=\"7ab0\">\n"
	daemon, input := legacySubmitFixture(t, &echo)
	const operation = "11111111-2222-4333-8444-555555555555"
	for attempt := 0; attempt < 2; attempt++ {
		receipt := legacySubmit(t, daemon, text, operation)
		if receipt["status"] != "accepted" || receipt["acceptance"] != "transcript" {
			t.Fatalf("wrapped provider history was not acknowledged: %v", receipt)
		}
	}
	if len(input.inputs) != 2 || input.session.ClaudeEventCount() != 1 {
		t.Fatalf("paste was resent: writes=%d user events=%d", len(input.inputs), input.session.ClaudeEventCount())
	}
}

func TestLegacyCompleteReceiptsPreserveRepeatedMessagesAndSubscribers(t *testing.T) {
	text := strings.Repeat("é🙂\n", 1024)
	daemon, input := legacySubmitFixture(t, &text)
	attachment := input.session.Attach(state.AttachOptions{})
	defer attachment.Cancel()
	for _, operation := range []string{"11111111-2222-4333-8444-555555555555", "11111111-2222-4333-8444-555555555556"} {
		receipt := legacySubmit(t, daemon, delivery.PasteStart+text+delivery.PasteEnd, operation)
		if receipt["status"] != "accepted" || receipt["acceptance"] != "transcript" {
			t.Fatalf("receipt=%v", receipt)
		}
		select {
		case event := <-attachment.Events:
			if event.Kind != proto.EventClaude {
				t.Fatalf("subscription event=%v", event.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("confirmation consumed or interrupted the subscriber's event")
		}
		replay := legacySubmit(t, daemon, delivery.PasteStart+text+delivery.PasteEnd, operation)
		if replay["status"] != "accepted" || replay["duplicate"] != true {
			t.Fatalf("replay=%v", replay)
		}
	}
	if len(input.inputs) != 4 || input.session.ClaudeEventCount() != 2 {
		t.Fatalf("writes=%d events=%d", len(input.inputs), input.session.ClaudeEventCount())
	}
}

func TestLegacyOldAcceptedReceiptIsNotCompleteDeliveryProof(t *testing.T) {
	daemon, input := legacySubmitFixture(t, nil)
	const operation = "11111111-2222-4333-8444-555555555555"
	if _, _, err := daemon.handler.deliveries.Begin(operation, "legacy-claude", "old input"); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.handler.deliveries.Complete(operation, delivery.StatusAccepted, true, false, ""); err != nil {
		t.Fatal(err)
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/message-deliveries/"+operation, nil, "127.0.0.1:4567", nil)
	var receipt map[string]any
	decodeBody(t, response, &receipt)
	if receipt["status"] != "unknown" || receipt["delivered"] != false || receipt["retry"] != false {
		t.Fatalf("old receipt=%v", receipt)
	}
	if len(input.inputs) != 0 {
		t.Fatal("receipt lookup sent input")
	}
	recorded, err := daemon.handler.deliveries.Get(operation)
	if err != nil || recorded.Status != delivery.StatusAccepted {
		t.Fatal("receipt projection rewrote historical state")
	}
}

func legacySubmitFixture(t *testing.T, echo *string) (testDaemon, *legacyEchoInput) {
	t.Helper()
	daemon := newTestDaemon(t)
	runner := prototest.NewRunner(proto.RunnerInfo{ID: "legacy-claude", Cmd: "claude", Cwd: daemon.root, Cols: 120, Rows: 40, ProtocolVersion: 2})
	session, err := daemon.registry.Register(context.Background(), runner, "Legacy Claude", "")
	if err != nil {
		t.Fatal(err)
	}
	input := &legacyEchoInput{sessionService: daemon.registry, session: session, echo: echo, test: t}
	daemon.handler.registry = input
	return daemon, input
}

func legacySubmit(t *testing.T, daemon testDaemon, text, operation string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"data": text, "operation_id": operation})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/legacy-claude/submit", strings.NewReader(string(body))).WithContext(ctx)
	request.Host = "127.0.0.1:8787"
	request.RemoteAddr = "127.0.0.1:4567"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	daemon.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK && response.Code != http.StatusNotFound {
		t.Fatalf("submit: %d %s", response.Code, response.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, response, &receipt)
	return receipt
}

func TestLegacyControlTextIsRefusedWithoutWriting(t *testing.T) {
	daemon, input := legacySubmitFixture(t, nil)
	receipt := legacySubmit(t, daemon, "literal\x1b[201~escape", "11111111-2222-4333-8444-555555555555")
	if receipt["status"] != "not-delivered" || receipt["retry"] != true || receipt["delivered"] != false || len(input.inputs) != 0 {
		t.Fatalf("receipt=%v writes=%d", receipt, len(input.inputs))
	}
}

func TestLegacyMuxSubmitRequiresTheCompleteProviderMessage(t *testing.T) {
	text := "BEGIN\n" + strings.Repeat("synthetic text ", 600) + "\nEND"
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			echo := text[len(text)-310:]
			if complete {
				echo = text
			}
			daemon, input := legacySubmitFixture(t, &echo)
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			accepted, reason := daemon.handler.submitMuxInput(ctx, "legacy-claude", text)
			if accepted != complete || (!complete && reason == "") {
				t.Fatalf("accepted=%v complete=%v reason=%q", accepted, complete, reason)
			}
			if len(input.inputs) != 2 || input.inputs[0] != delivery.PasteStart+text+delivery.PasteEnd || input.inputs[1] != "\r" {
				t.Fatal("mux did not send exactly one framed message and Enter")
			}
		})
	}
}

func TestLegacyClaudeSubmitUsesOneCompletePasteEnvelope(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025, 4087, 4088, 4089, 4095, 4096, 4097, 4394, 8192, 16384} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			text := "BEGIN\n" + strings.Repeat("x", size-10) + "\nEND"
			daemon, input := legacySubmitFixture(t, &text)
			receipt := legacySubmit(t, daemon, text, "11111111-2222-4333-8444-555555555555")
			if len(input.inputs) != 2 || input.inputs[0] != "\x1b[200~"+text+"\x1b[201~" || input.inputs[1] != "\r" {
				t.Fatalf("message %d not framed as one paste: inputs=%d first bytes=%d", size, len(input.inputs), len(input.inputs[0]))
			}
			if receipt["status"] != "accepted" || receipt["acceptance"] != "transcript" {
				t.Fatalf("receipt=%v", receipt)
			}
		})
	}
}

func TestLegacyClaudeSubmitDoesNotClaimSuffixOrOldEventDelivered(t *testing.T) {
	text := "BEGIN\n" + strings.Repeat("é🙂fixture ", 600) + "\nEND"
	for _, echo := range []string{text[len(text)-310:], "another user message", ""} {
		t.Run(fmt.Sprint(len(echo)), func(t *testing.T) {
			daemon, input := legacySubmitFixture(t, &echo)
			recordUserEvent(t, input.session, time.Now().Add(-time.Minute), text)
			receipt := legacySubmit(t, daemon, text, "11111111-2222-4333-8444-555555555555")
			if receipt["status"] != "unknown" || receipt["delivered"] != false || receipt["retry"] != false {
				t.Fatalf("partial/unrelated/old message falsely accepted: %v", receipt)
			}
			if len(input.inputs) != 2 {
				t.Fatalf("unexpected replay: %v", input.inputs)
			}
		})
	}
}

func (r *legacyEchoInput) Input(ctx context.Context, id, data string) bool {
	r.inputs = append(r.inputs, data)
	if data == "\r" && r.echo != nil {
		recordUserEvent(r.test, r.session, time.Now(), *r.echo)
	}
	return r.sessionService.Input(ctx, id, data)
}
