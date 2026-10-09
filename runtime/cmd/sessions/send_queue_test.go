package main

import "testing"

func TestSendQueueAcknowledgementIsFinalNotProviderConfirmation(t *testing.T) {
	result, final := acknowledgedSendResult(deliveryReceipt{
		Status: "accepted", Acceptance: "queue", Delivered: true, OperationID: "queued", Reason: "saved, but paused; use Retry",
	}, "claude-code")
	if !final || result.Confidence != "queue-accepted" || result.ExitCode != 0 || result.Confirmed == nil || !*result.Confirmed {
		t.Fatalf("queue receipt entered polling or hid its boundary: %+v, final %v", result, final)
	}
	if result.Reason != "saved, but paused; use Retry" {
		t.Fatal("queued acknowledgment hid the recovery action")
	}
}
