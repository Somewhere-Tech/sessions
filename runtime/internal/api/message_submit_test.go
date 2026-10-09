package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
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
	info, _ := registerStructuredRunner(t, daemon, id)
	return info
}

// registerStructuredRunner also hands back the runner, so a test can make it
// answer after its caller is gone.
func registerStructuredRunner(t *testing.T, daemon testDaemon, id string) (state.SessionInfo, *prototest.Runner) {
	t.Helper()
	runner := prototest.NewRunner(proto.RunnerInfo{
		ID: id, Cmd: "codex", Cwd: daemon.root, Cols: 120, Rows: 40, ProtocolVersion: proto.ProtocolVersion,
		MessageSubmit: true,
	})
	session, err := daemon.registry.Register(context.Background(), runner, "Structured", "")
	if err != nil {
		t.Fatal(err)
	}
	return session.Info(), runner
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

// Codex applied the exact steer and then answered with an internal error, so
// the runner reports an unknown boundary. The receipt must stay unknown and
// non-retryable, and the same operation must read that receipt rather than
// steer the provider a second time.
func TestUnknownSteerAfterProviderErrorIsNeverSubmittedAgain(t *testing.T) {
	daemon := newTestDaemon(t)
	session := registerStructuredSession(t, daemon, "structured-steer-error")
	const operationID = "20000000-0000-4000-8000-000000000003"
	service := &structuredMessageService{
		sessionService: daemon.registry,
		result: proto.MessageResult{OperationID: operationID, Boundary: "unknown",
			Error: "Codex did not confirm steering: steer Codex turn: JSON-RPC error -32603: internal error"},
	}
	daemon.handler.registry = service
	steer := func() map[string]any {
		body := `{"data":"keep the exact text","operation_id":"` + operationID + `","mode":"steer"}`
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+session.ID+"/submit", strings.NewReader(body), "127.0.0.1:4567", nil)
		var receipt map[string]any
		decodeBody(t, response, &receipt)
		return receipt
	}
	first, second := steer(), steer()
	for _, receipt := range []map[string]any{first, second} {
		if receipt["status"] != "unknown" || receipt["retry"] != false || receipt["delivered"] != false {
			t.Fatalf("receipt = %#v", receipt)
		}
	}
	if first["duplicate"] != false || second["duplicate"] != true {
		t.Fatalf("duplicate flags = first %v, second %v", first["duplicate"], second["duplicate"])
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.calls) != 1 || service.calls[0].Text != "keep the exact text" || service.calls[0].Mode != "steer" {
		t.Fatalf("structured calls = %#v, want the original steer exactly once", service.calls)
	}
	if len(service.inputCalls) != 0 {
		t.Fatalf("unknown steer fell back to terminal input: %#v", service.inputCalls)
	}
}

// One operation, accepted at the provider boundary, whose answer never reached
// the client. The runner commits the turn before it acknowledges, so a caller
// that disconnects mid-request leaves a delivered message recorded as unknown.
// Asking again must recover the runner's own answer and must never send twice.
func TestInterruptedSubmitRecoversTheRunnersLateAcknowledgement(t *testing.T) {
	daemon := newTestDaemon(t)
	session, runner := registerStructuredRunner(t, daemon, "structured-late-ack")
	const operationID = "22222222-3333-4444-8555-666666666666"
	service := &structuredMessageService{
		sessionService: daemon.registry,
		err:            context.Canceled,
		result:         proto.MessageResult{OperationID: operationID},
	}
	daemon.handler.registry = service

	// The client disappears while the runner is already committing the message.
	first := submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship the release")
	if first["status"] != "unknown" || first["delivered"] != false || first["retry"] != false {
		t.Fatalf("interrupted receipt = %#v", first)
	}
	if reread := readDeliveryReceipt(t, daemon.handler, operationID); reread["status"] != "unknown" {
		t.Fatalf("re-read before the runner answered = %#v", reread)
	}

	// The runner answers a moment later, correlated by the same operation id.
	runner.AcknowledgeLate(proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "provider"})

	reread := readDeliveryReceipt(t, daemon.handler, operationID)
	if reread["status"] != "accepted" || reread["delivered"] != true || reread["acceptance"] != "provider" || reread["retry"] != false {
		t.Fatalf("recovered receipt = %#v", reread)
	}

	// Retrying the same operation reads that evidence instead of sending again.
	retry := submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship the release")
	if retry["duplicate"] != true || retry["status"] != "accepted" || retry["acceptance"] != "provider" {
		t.Fatalf("retry receipt = %#v", retry)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.calls) != 1 {
		t.Fatalf("structured calls = %d, want exactly one execution", len(service.calls))
	}
	if len(service.inputCalls) != 0 {
		t.Fatalf("recovery fell back to terminal input: %#v", service.inputCalls)
	}
}

// A late answer that says the message was refused is still uncertainty for the
// original request: nothing here may turn it into an after-the-fact instruction
// to send the message again.
func TestInterruptedSubmitKeepsUncertaintyForALateRefusal(t *testing.T) {
	daemon := newTestDaemon(t)
	session, runner := registerStructuredRunner(t, daemon, "structured-late-refusal")
	const operationID = "33333333-4444-4555-8666-777777777777"
	service := &structuredMessageService{
		sessionService: daemon.registry,
		err:            context.Canceled,
		result:         proto.MessageResult{OperationID: operationID},
	}
	daemon.handler.registry = service

	submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship the release")
	runner.AcknowledgeLate(proto.MessageResult{
		OperationID: operationID, Boundary: "runner", Error: "Claude cannot accept this message during an active turn.",
	})

	reread := readDeliveryReceipt(t, daemon.handler, operationID)
	if reread["status"] != "unknown" || reread["delivered"] != false || reread["retry"] != false {
		t.Fatalf("late refusal receipt = %#v", reread)
	}
}

func readDeliveryReceipt(t *testing.T, server *Server, operationID string) map[string]any {
	t.Helper()
	response := serve(t, server, http.MethodGet, "/api/message-deliveries/"+operationID, nil, "127.0.0.1:4567", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delivery re-read = %d %s", response.Code, response.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, response, &receipt)
	return receipt
}

// restartedDaemon serves the same durable state with none of the previous
// process's in-memory evidence: a fresh registry, no sessions, no runner cache.
func restartedDaemon(t *testing.T, daemon testDaemon) testDaemon {
	t.Helper()
	launcher := prototest.NewLauncher()
	registry := state.NewRegistry(daemon.config, launcher)
	return testDaemon{
		config: daemon.config, registry: registry, launcher: launcher,
		handler: New(daemon.config, registry), root: daemon.root,
	}
}

// Once late evidence has settled a receipt, the answer lives on disk. Losing the
// daemon-side cache that produced it — a restart, or a replacement runner that
// never saw the operation — must not walk the receipt back to uncertainty.
func TestConfirmedReceiptOutlivesTheCachedEvidenceThatSettledIt(t *testing.T) {
	daemon := newTestDaemon(t)
	session, runner := registerStructuredRunner(t, daemon, "structured-durable")
	const operationID = "44444444-5555-4666-8777-888888888888"
	daemon.handler.registry = &structuredMessageService{
		sessionService: daemon.registry, err: context.Canceled,
		result: proto.MessageResult{OperationID: operationID},
	}

	submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship the release")
	runner.AcknowledgeLate(proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "provider"})
	if receipt := readDeliveryReceipt(t, daemon.handler, operationID); receipt["status"] != "accepted" {
		t.Fatalf("receipt after late evidence = %#v", receipt)
	}

	// The durable record, read by a Store that never saw any of this.
	stored, err := delivery.New(daemon.config.StateRoot).Get(operationID)
	if err != nil {
		t.Fatalf("read durable receipt: %v", err)
	}
	if stored.Status != delivery.StatusAccepted || !stored.Delivered || stored.Retry || stored.Acceptance != "provider" {
		t.Fatalf("durable record = %+v", stored)
	}

	// A restarted daemon has no cached acknowledgment at all.
	restarted := restartedDaemon(t, daemon)
	if receipt := readDeliveryReceipt(t, restarted.handler, operationID); receipt["status"] != "accepted" ||
		receipt["delivered"] != true || receipt["acceptance"] != "provider" {
		t.Fatalf("receipt after restart = %#v", receipt)
	}

	// Neither does a replacement runner for the same session.
	registerStructuredRunner(t, restarted, session.ID)
	if receipt := readDeliveryReceipt(t, restarted.handler, operationID); receipt["status"] != "accepted" ||
		receipt["acceptance"] != "provider" {
		t.Fatalf("receipt after the runner was replaced = %#v", receipt)
	}
}

// Without evidence there is no answer, and no answer is not permission to send
// the message a second time.
func TestUnsettledReceiptStaysUnknownAndNeverResubmitsAfterRestart(t *testing.T) {
	daemon := newTestDaemon(t)
	session, _ := registerStructuredRunner(t, daemon, "structured-unsettled")
	const operationID = "55555555-6666-4777-8888-999999999999"
	daemon.handler.registry = &structuredMessageService{
		sessionService: daemon.registry, err: context.Canceled,
		result: proto.MessageResult{OperationID: operationID},
	}
	submitMessageRequest(t, daemon.handler, session.ID, operationID, "ship the release")

	restarted := restartedDaemon(t, daemon)
	registerStructuredRunner(t, restarted, session.ID)
	service := &structuredMessageService{
		sessionService: restarted.registry,
		result:         proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "provider"},
	}
	restarted.handler.registry = service

	if receipt := readDeliveryReceipt(t, restarted.handler, operationID); receipt["status"] != "unknown" ||
		receipt["delivered"] != false || receipt["retry"] != false || receipt["acceptance"] != "" {
		t.Fatalf("receipt without evidence = %#v", receipt)
	}
	retry := submitMessageRequest(t, restarted.handler, session.ID, operationID, "ship the release")
	if retry["duplicate"] != true || retry["status"] != "unknown" || retry["retry"] != false {
		t.Fatalf("retry receipt = %#v", retry)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.calls) != 0 {
		t.Fatalf("an unsettled operation was submitted again: %#v", service.calls)
	}
	if len(service.inputCalls) != 0 {
		t.Fatalf("an unsettled operation fell back to terminal input: %#v", service.inputCalls)
	}
}
