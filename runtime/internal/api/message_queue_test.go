package api

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestStructuredQueueReceiptSurvivesDuplicateWithoutResending(t *testing.T) {
	daemon := newTestDaemon(t)
	session := registerStructuredSession(t, daemon, "structured-queue")
	const operationID = "11111111-2222-4333-8444-555555555555"
	service := &structuredMessageService{sessionService: daemon.registry,
		result: proto.MessageResult{OperationID: operationID, Accepted: true, Boundary: "queue"}}
	daemon.handler.registry = service
	for range 2 {
		receipt := submitMessageRequest(t, daemon.handler, session.ID, operationID, "next turn")
		if receipt["status"] != "accepted" || receipt["acceptance"] != "queue" || receipt["retry"] != false {
			t.Fatalf("queued receipt = %#v", receipt)
		}
	}
	if len(service.calls) != 1 || len(service.inputCalls) != 0 {
		t.Fatalf("queue acceptance resent a message: %d submits, %d terminal inputs", len(service.calls), len(service.inputCalls))
	}
}
