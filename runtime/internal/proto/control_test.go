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
