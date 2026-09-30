package proto

import "testing"

func TestModelControlRoundTripAndOrdinaryInput(t *testing.T) {
	encoded, err := EncodeModelControl(ModelControl{Model: "gpt-5.6-sol", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := DecodeModelControl(encoded)
	if err != nil {
		t.Fatalf("DecodeModelControl() = %#v, %v", control, err)
	}
	if control.Model != "gpt-5.6-sol" || control.Effort != "high" {
		t.Fatalf("control = %#v", control)
	}
	if _, err := DecodeModelControl([]byte(`{"model":"gpt"} trailing`)); err == nil {
		t.Fatal("trailing payload accepted")
	}
}

func TestApprovalControlRoundTripRejectsBadDecisions(t *testing.T) {
	payload, err := EncodeApprovalControl(ApprovalControl{ID: "approval-1", Decision: ApprovalAllowForSession, By: "manager-1"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := DecodeApprovalControl(payload)
	if err != nil || control.ID != "approval-1" || control.Decision != ApprovalAllowForSession || control.By != "manager-1" {
		t.Fatalf("decoded = %#v, %v", control, err)
	}
	if _, err := EncodeApprovalControl(ApprovalControl{ID: "approval-1", Decision: "maybe"}); err == nil {
		t.Fatal("encoded an unknown decision")
	}
	if _, err := EncodeApprovalControl(ApprovalControl{Decision: ApprovalAllow}); err == nil {
		t.Fatal("encoded a control without an id")
	}
	if _, err := DecodeApprovalControl([]byte(`{"id":"a","decision":"allow","extra":1}`)); err == nil {
		t.Fatal("decoded unknown fields")
	}
	if _, err := DecodeApprovalControl([]byte(`{"id":"a","decision":"later"}`)); err == nil {
		t.Fatal("decoded an unknown decision")
	}
}
