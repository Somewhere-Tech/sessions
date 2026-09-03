package proto

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// ModelControl changes the defaults used by the next structured-provider turn.
// It travels in a protocol-v2 request frame and is acknowledged by the runner
// only after its provider state and durable metadata both accept the change.
type ModelControl struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

type ModelControlResult struct {
	Error string `json:"error,omitempty"`
}

type RetryControlResult struct {
	Error string `json:"error,omitempty"`
}

func EncodeModelControl(control ModelControl) ([]byte, error) {
	return json.Marshal(control)
}

func DecodeModelControl(data []byte) (ModelControl, error) {
	var control ModelControl
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return ModelControl{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ModelControl{}, errors.New("model control contains trailing JSON")
		}
		return ModelControl{}, err
	}
	return control, nil
}

// ApprovalControl answers one approval a structured runner is holding open.
// ID is the approval id the runner announced in its approval_requested event;
// Decision is allow, allow-session, or deny; By is the session id of the lane
// that decided, empty when a person did.
type ApprovalControl struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	By       string `json:"by,omitempty"`
}

const (
	ApprovalAllow           = "allow"
	ApprovalAllowForSession = "allow-session"
	ApprovalDeny            = "deny"
)

// ValidApprovalDecision reports whether value is a decision a runner accepts.
func ValidApprovalDecision(value string) bool {
	return value == ApprovalAllow || value == ApprovalAllowForSession || value == ApprovalDeny
}

func EncodeApprovalControl(control ApprovalControl) ([]byte, error) {
	if control.ID == "" {
		return nil, errors.New("approval control needs an id")
	}
	if !ValidApprovalDecision(control.Decision) {
		return nil, errors.New("approval decision must be allow, allow-session, or deny")
	}
	return json.Marshal(control)
}

func DecodeApprovalControl(data []byte) (ApprovalControl, error) {
	var control ApprovalControl
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil {
		return ApprovalControl{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ApprovalControl{}, errors.New("approval control contains trailing JSON")
		}
		return ApprovalControl{}, err
	}
	if control.ID == "" || !ValidApprovalDecision(control.Decision) {
		return ApprovalControl{}, errors.New("approval control needs an id and a valid decision")
	}
	return control, nil
}
