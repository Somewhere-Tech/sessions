package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
)

func TestLateLegacyReceiptSettlesWithoutInputIncludingReloadedStore(t *testing.T) {
	daemon, input := legacySubmitFixture(t, nil)
	const operation = "11111111-2222-4333-8444-555555555555"
	const text = "Please inspect the entire Unicode request 🙂\nsecond line"
	recordUserEvent(t, input.session, time.Now().Add(-time.Minute), text)
	receipt := legacySubmit(t, daemon, text, operation)
	if receipt["status"] != "unknown" {
		t.Fatalf("receipt=%v", receipt)
	}
	recordUserEvent(t, input.session, time.Now().Add(-time.Minute), text)
	check := func(want string) {
		t.Helper()
		response := serve(t, daemon.handler, http.MethodGet, "/api/message-deliveries/"+operation, nil, "127.0.0.1:4567", nil)
		var body map[string]any
		decodeBody(t, response, &body)
		if body["status"] != want {
			t.Fatalf("receipt=%v, want=%s", body, want)
		}
	}
	check("unknown") // Even a replay after the cursor is not fresh acceptance.
	recordUserEvent(t, input.session, time.Now(), "second line")
	check("unknown") // A suffix is not the original message.
	daemon.handler.deliveries = delivery.New(daemon.handler.config.StateRoot)
	recordUserEvent(t, input.session, time.Now(), text)
	check("accepted")
	check("accepted")
	replayed := legacySubmit(t, daemon, text, operation)
	if replayed["status"] != "accepted" || replayed["duplicate"] != true || len(input.inputs) != 2 {
		t.Fatalf("lookup or replay sent input: receipt=%v writes=%v", replayed, input.inputs)
	}
}

func TestLateLegacyUnknownDoesNotAcceptMissingOrReplacedAnchor(t *testing.T) {
	daemon, input := legacySubmitFixture(t, nil)
	const operation = "11111111-2222-4333-8444-555555555555"
	recordUserEvent(t, input.session, time.Now().Add(-time.Minute), "earlier message")
	legacySubmit(t, daemon, "the original request", operation)
	record, err := daemon.handler.deliveries.Get(operation)
	if err != nil {
		t.Fatal(err)
	}
	record.Transcript.Anchor = "different replay generation"
	recordUserEvent(t, input.session, time.Now(), "the original request")
	if resolved := daemon.handler.reconcileLateAcceptance(record); resolved.Status != delivery.StatusUnknown {
		t.Fatalf("replaced history falsely accepted: %+v", resolved)
	}
	if len(input.inputs) != 2 {
		t.Fatal("reconciliation sent input")
	}
}
