package api

import (
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
)

// A refusal is an answer. Reading the receipt for a message the provider
// refused before any input reached it must return that receipt, not a 404 that
// callers have to unwrap from an error string to find the refusal inside.
func TestReadingARefusedReceiptReturnsIt(t *testing.T) {
	daemon := newTestDaemon(t)
	const operationID = "55555555-6666-4777-8888-999999999999"
	const reason = "Claude cannot accept this message during an active turn. Wait for the turn to finish and send again."
	if _, _, err := daemon.handler.deliveries.Begin(operationID, "target-session", "look at this brief"); err != nil {
		t.Fatal(err)
	}
	// Refused before input: nothing reached the provider, so retrying this
	// exact operation once the turn ends is safe.
	if _, err := daemon.handler.deliveries.Complete(
		operationID, delivery.StatusNotDelivered, false, true, reason); err != nil {
		t.Fatal(err)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/api/message-deliveries/"+operationID, nil, "127.0.0.1:4567", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("reading a recorded refusal = %d %s", response.Code, response.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, response, &receipt)
	for field, want := range map[string]any{
		"operation_id": operationID,
		"session_id":   "target-session",
		"status":       string(delivery.StatusNotDelivered),
		"delivered":    false,
		"retry":        true,
		"reason":       reason,
		"duplicate":    true,
	} {
		if receipt[field] != want {
			t.Errorf("receipt[%q] = %#v, want %#v", field, receipt[field], want)
		}
	}
	if receipt["created_at_ms"] == nil || receipt["updated_at_ms"] == nil {
		t.Errorf("receipt is missing its timestamps: %#v", receipt)
	}
}

// 404 keeps its meaning: this daemon never recorded that operation.
func TestReadingAnUnrecordedOperationIsNotFound(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(t, daemon.handler, http.MethodGet,
		"/api/message-deliveries/66666666-7777-4888-8999-aaaaaaaaaaaa", nil, "127.0.0.1:4567", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("reading an operation nobody recorded = %d %s", response.Code, response.Body.String())
	}
}

// Every recorded status reads back at 200, including the ones that say the
// message did not arrive or that Sessions cannot tell.
func TestEveryRecordedStatusReadsBackAsAReceipt(t *testing.T) {
	daemon := newTestDaemon(t)
	for _, test := range []struct {
		operationID string
		status      delivery.Status
		delivered   bool
		retry       bool
		want        string
	}{
		{"11111111-1111-4111-8111-111111111111", delivery.StatusAccepted, true, false, "accepted"},
		{"22222222-2222-4222-8222-222222222222", delivery.StatusNotDelivered, false, true, "not-delivered"},
		{"33333333-3333-4333-8333-333333333333", delivery.StatusUnknown, false, false, "unknown"},
		{"44444444-4444-4444-8444-444444444444", delivery.StatusTextOnly, false, false, "text-delivered"},
	} {
		t.Run(test.want, func(t *testing.T) {
			if _, _, err := daemon.handler.deliveries.Begin(test.operationID, "target-session", "a message"); err != nil {
				t.Fatal(err)
			}
			if _, err := daemon.handler.deliveries.Complete(
				test.operationID, test.status, test.delivered, test.retry, "recorded"); err != nil {
				t.Fatal(err)
			}
			response := serve(t, daemon.handler, http.MethodGet,
				"/api/message-deliveries/"+test.operationID, nil, "127.0.0.1:4567", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("%s receipt = %d %s", test.want, response.Code, response.Body.String())
			}
			var receipt map[string]any
			decodeBody(t, response, &receipt)
			if receipt["status"] != test.want {
				t.Fatalf("status = %#v, want %q", receipt["status"], test.want)
			}
		})
	}
}
