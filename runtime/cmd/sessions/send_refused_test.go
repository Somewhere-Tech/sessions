package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const refusedSessionID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"

// The refusal a structured Claude session gives while it is mid-turn: recorded,
// definitive, and safe to retry once the turn ends.
const activeTurnRefusal = "Claude cannot accept this message during an active turn. Wait for the turn to finish and send again."

// refusedDaemon answers the submit with a receipt for a message that was
// refused before any input reached the provider. httpStatus is what the daemon
// answers with, so the same test can describe the daemon before and after this
// slice.
func refusedDaemon(t *testing.T, httpStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		var operationID string
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": refusedSessionID, "cmd": "claude", "tool": "claude",
				"kind": "claude-structured", "messageSubmit": true, "working": true,
			}}})
			return
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+refusedSessionID+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
			return
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+refusedSessionID+"/submit":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			operationID = body["operation_id"]
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/message-deliveries/"):
			operationID = strings.TrimPrefix(request.URL.Path, "/api/message-deliveries/")
		default:
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(httpStatus)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"operation_id": operationID, "session_id": refusedSessionID,
			"status": "not-delivered", "delivered": false, "retry": true,
			"reason": activeTurnRefusal, "duplicate": false,
			"created_at_ms": 1757000000000, "updated_at_ms": 1757000000001,
		})
	}))
}

// A refused send says what the daemon said, and says it was not delivered.
func TestSendPrintsTheDaemonsRefusal(t *testing.T) {
	for _, httpStatus := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(httpStatus), func(t *testing.T) {
			server := refusedDaemon(t, httpStatus)
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "send", refusedSessionID, "look at this brief"},
				strings.NewReader(""), &stdout, &stderr)
			t.Logf("exit=%d\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
			if code == 0 {
				t.Fatalf("a refused send exited 0")
			}
			if !strings.Contains(stderr.String(), activeTurnRefusal) {
				t.Errorf("stderr does not carry the daemon's own sentence: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), "not delivered") {
				t.Errorf("stderr does not say the message was not delivered: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), "retry") {
				t.Errorf("stderr does not say whether retrying is safe: %q", stderr.String())
			}
		})
	}
}

// send-status prints a refused receipt as a receipt, not as an error.
func TestSendStatusPrintsARefusedReceipt(t *testing.T) {
	const operationID = "22222222-3333-4444-8555-666666666666"
	for _, httpStatus := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(httpStatus), func(t *testing.T) {
			server := refusedDaemon(t, httpStatus)
			defer server.Close()
			t.Setenv("HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "send-status", operationID}, strings.NewReader(""), &stdout, &stderr)
			t.Logf("text exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			if code != 0 {
				t.Errorf("send-status on a recorded receipt exited %d: %q", code, stderr.String())
			}
			for _, fragment := range []string{"not-delivered", operationID, activeTurnRefusal} {
				if !strings.Contains(stdout.String(), fragment) {
					t.Errorf("send-status output %q does not contain %q", stdout.String(), fragment)
				}
			}

			var jsonOut, jsonErr bytes.Buffer
			jsonCode := run([]string{"--host", server.URL, "--json", "send-status", operationID},
				strings.NewReader(""), &jsonOut, &jsonErr)
			t.Logf("json exit=%d stdout=%q", jsonCode, jsonOut.String())
			var receipt deliveryReceipt
			if err := json.Unmarshal(jsonOut.Bytes(), &receipt); err != nil {
				t.Fatalf("--json output is not a receipt: %v (%q)", err, jsonOut.String())
			}
			if receipt.Status != "not-delivered" || receipt.OperationID != operationID || !receipt.Retry {
				t.Errorf("--json receipt = %+v", receipt)
			}
			if receipt.Reason != activeTurnRefusal {
				t.Errorf("--json receipt reason = %q", receipt.Reason)
			}
		})
	}
}
