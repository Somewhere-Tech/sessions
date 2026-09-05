package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type structuredMessageService struct {
	sessionService
	mu         sync.Mutex
	result     proto.MessageResult
	err        error
	calls      []proto.MessageControl
	inputCalls []string
}

func (s *structuredMessageService) SubmitMessage(_ context.Context, _ string, control proto.MessageControl, _ state.InputAttribution) (proto.MessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, control)
	return s.result, s.err
}

func (s *structuredMessageService) Input(ctx context.Context, id, data string) bool {
	s.mu.Lock()
	s.inputCalls = append(s.inputCalls, data)
	s.mu.Unlock()
	return s.sessionService.Input(ctx, id, data)
}

func registerStructuredSession(t *testing.T, daemon testDaemon, id string) state.SessionInfo {
	t.Helper()
	runner := prototest.NewRunner(proto.RunnerInfo{
		ID: id, Cmd: "codex", Cwd: daemon.root, Cols: 120, Rows: 40, ProtocolVersion: proto.ProtocolVersion,
		MessageSubmit: true,
	})
	session, err := daemon.registry.Register(context.Background(), runner, "Structured", "")
	if err != nil {
		t.Fatal(err)
	}
	return session.Info()
}

func submitMessageRequest(t *testing.T, server *Server, sessionID, operationID, text string) map[string]any {
	t.Helper()
	body := `{"data":"` + text + `","operation_id":"` + operationID + `"}`
	response := serve(t, server, http.MethodPost, "/api/sessions/"+sessionID+"/submit", strings.NewReader(body), "127.0.0.1:4567", nil)
	if response.Code != http.StatusOK && response.Code != http.StatusNotFound {
		t.Fatalf("submit = %d %s", response.Code, response.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, response, &receipt)
	return receipt
}

func TestStructuredSubmitReturnsCorrelatedReceipt(t *testing.T) {
	daemon := newTestDaemon(t)
	session := registerStructuredSession(t, daemon, "structured-correlated")
	const operationID = "11111111-2222-4333-8444-555555555555"
	service := &structuredMessageService{
		sessionService: daemon.registry,
		result:         proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "provider"},
	}
	daemon.handler.registry = service

	receipt := submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship release")
	if receipt["operation_id"] != operationID || receipt["status"] != "accepted" || receipt["acceptance"] != "provider" || receipt["delivered"] != true {
		t.Fatalf("receipt = %#v", receipt)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.calls) != 1 || service.calls[0].OperationID != operationID || service.calls[0].Text != "ship release" {
		t.Fatalf("structured calls = %#v", service.calls)
	}
	if len(service.inputCalls) != 0 {
		t.Fatalf("structured submit fell back to terminal input: %#v", service.inputCalls)
	}
}

func TestStructuredSubmitDuplicateOperationDoesNotSubmitTwice(t *testing.T) {
	daemon := newTestDaemon(t)
	session := registerStructuredSession(t, daemon, "structured-duplicate")
	const operationID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	service := &structuredMessageService{
		sessionService: daemon.registry,
		result:         proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "runner"},
	}
	daemon.handler.registry = service

	first := submitMessageRequest(t, daemon.handler, session.ID, operationID, "only once")
	second := submitMessageRequest(t, daemon.handler, session.ID, operationID, "only once")
	if first["duplicate"] != false || second["duplicate"] != true || second["status"] != "accepted" {
		t.Fatalf("receipts = first %#v, second %#v", first, second)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.calls) != 1 {
		t.Fatalf("structured calls = %d, want 1", len(service.calls))
	}
	if len(service.inputCalls) != 0 {
		t.Fatalf("duplicate fell back to terminal input: %#v", service.inputCalls)
	}
}

func TestStructuredSubmitRefusalAndUnknownNeverBecomeSuccessOrTerminalInput(t *testing.T) {
	for _, test := range []struct {
		name       string
		operation  string
		result     proto.MessageResult
		wantStatus string
		wantRetry  bool
	}{
		{
			name: "refused", operation: "10000000-0000-4000-8000-000000000001",
			result:     proto.MessageResult{OperationID: "10000000-0000-4000-8000-000000000001", Boundary: "runner", Error: "provider refused message"},
			wantStatus: "not-delivered", wantRetry: true,
		},
		{
			name: "unknown", operation: "10000000-0000-4000-8000-000000000002",
			result:     proto.MessageResult{OperationID: "10000000-0000-4000-8000-000000000002", Boundary: "unknown", Error: "acknowledgement lost"},
			wantStatus: "unknown", wantRetry: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			daemon := newTestDaemon(t)
			session := registerStructuredSession(t, daemon, "structured-"+test.name)
			service := &structuredMessageService{sessionService: daemon.registry, result: test.result}
			daemon.handler.registry = service

			receipt := submitMessageRequest(t, daemon.handler, session.ID, test.operation, "do not type Enter")
			if receipt["status"] != test.wantStatus || receipt["delivered"] != false || receipt["retry"] != test.wantRetry {
				t.Fatalf("receipt = %#v", receipt)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if len(service.calls) != 1 {
				t.Fatalf("structured calls = %d, want 1", len(service.calls))
			}
			if len(service.inputCalls) != 0 {
				t.Fatalf("%s result fell back to terminal input: %#v", test.name, service.inputCalls)
			}
		})
	}
}
