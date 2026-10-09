package state

import (
	"fmt"
	"regexp"
)

// A start receipt answers "did the work I delegated actually begin?" from
// evidence Sessions already holds, one step at a time:
//
//	created → first request delivered → working → completed, or blocked
//
// Each step is claimed only from its own evidence. A successful create says
// nothing about the first request, an accepted request says nothing about the
// provider taking a turn, and an HTTP answer is never read as either. Process
// liveness (exited, unreachable, runnerGone) stays on SessionInfo where it has
// always been; the receipt describes the task, not the process.
//
// The receipt is present only for sessions created with an operation id, so
// every earlier session and client is unchanged.

const (
	StartPhaseCreated            = "created"
	StartPhasePromptNotDelivered = "prompt-not-delivered"
	StartPhasePromptUnknown      = "prompt-unknown"
	StartPhasePromptDelivered    = "prompt-delivered"
	StartPhaseWorking            = "working"
	StartPhaseCompleted          = "completed"
	StartPhaseBlocked            = "blocked"

	// Prompt statuses extend the delivery receipt statuses with the two answers
	// only a start receipt can give: no receipt exists yet, and the daemon is
	// delivering it right now.
	StartPromptNotSent      = "not-sent"
	StartPromptSending      = "sending"
	StartPromptAccepted     = "accepted"
	StartPromptNotDelivered = "not-delivered"
	StartPromptUnknown      = "unknown"
	StartPromptTextOnly     = "text-delivered"
	// StartPromptUnreadable means the receipt exists but could not be read.
	// It is reported, never mistaken for "not sent".
	StartPromptUnreadable = "unreadable"

	StartRecoverySendPrompt     = "send-prompt"
	StartRecoveryInspect        = "inspect"
	StartRecoveryConnectAccount = "connect-account"
	StartRecoveryAnswer         = "answer"
	StartRecoveryRetryTurn      = "retry-turn"
	StartRecoveryWait           = "wait"
	StartRecoveryStartNew       = "start-new"
)

var startOperationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidateStartOperationIDs checks the optional create idempotency key and the
// first-request delivery operation id. Both use the delivery receipt format so
// one id can be looked up with `sessions send-status`.
func ValidateStartOperationIDs(operationID, promptOperationID string) error {
	if operationID != "" && !startOperationIDPattern.MatchString(operationID) {
		return fmt.Errorf("operation_id must be a lowercase UUID v4")
	}
	if promptOperationID != "" && !startOperationIDPattern.MatchString(promptOperationID) {
		return fmt.Errorf("prompt_operation_id must be a lowercase UUID v4")
	}
	if operationID != "" && operationID == promptOperationID {
		return fmt.Errorf("prompt_operation_id must differ from operation_id")
	}
	return nil
}

// StartReceipt is the additive `start` object on SessionInfo.
type StartReceipt struct {
	OperationID       string `json:"operation_id,omitempty"`
	PromptOperationID string `json:"prompt_operation_id,omitempty"`
	Phase             string `json:"phase"`
	// Prompt is the first request's delivery, omitted when the session was
	// created without one.
	Prompt *StartPrompt `json:"prompt,omitempty"`
	// Evidence is the fact the phase rests on, in Sessions' words.
	Evidence string `json:"evidence"`
	// EvidenceSource names where that fact came from: delivery-receipt,
	// provider-events, terminal, provider-fault, approval, launch, or session.
	EvidenceSource string `json:"evidence_source"`
	// BlockedBy is set only in the blocked phase: a provider failure kind
	// (auth, provider-unavailable, rate-limited, other), approval, or
	// needs-input, or runner-unavailable.
	BlockedBy string         `json:"blocked_by,omitempty"`
	Recovery  *StartRecovery `json:"recovery,omitempty"`
	// Replayed marks a create response that returned the session an earlier
	// request with the same operation id already created.
	Replayed bool `json:"replayed,omitempty"`
}

// StartPrompt is the first request's delivery state. Retry keeps the delivery
// receipt's meaning exactly: true only when Sessions proved nothing reached the
// provider, so sending the same request again cannot duplicate it.
type StartPrompt struct {
	Status     string `json:"status"`
	Acceptance string `json:"acceptance,omitempty"`
	Retry      bool   `json:"retry"`
	Reason     string `json:"reason,omitempty"`
	// At is when the receipt last changed, Unix epoch milliseconds.
	At int64 `json:"at,omitempty"`
}

// StartRecovery is the one next action that cannot duplicate the work.
type StartRecovery struct {
	Action  string `json:"action"`
	Command string `json:"command,omitempty"`
	Detail  string `json:"detail"`
}

// StartCreateReplayError reports that an operation id already created a
// session that can no longer be returned as the live answer. Creating another
// would duplicate the work, so the caller is told which session it was.
type StartCreateReplayError struct {
	OperationID string
	SessionID   string
	Reason      string
}

func (e *StartCreateReplayError) Error() string {
	return fmt.Sprintf("operation %s already created session %s, which %s; inspect it with `sessions status %s` instead of starting the same work again",
		e.OperationID, e.SessionID, e.Reason, e.SessionID)
}

// StartCreateFailedError is a create that failed after the session was
// recorded. Its launch may or may not have produced a runner, so the caller
// gets the session id to inspect rather than a bare error to retry blindly.
type StartCreateFailedError struct {
	OperationID string
	SessionID   string
	Err         error
}

func (e *StartCreateFailedError) Error() string {
	return fmt.Sprintf("session %s was recorded, but runner startup was not confirmed: %v; inspect it with `sessions status %s` before creating another",
		e.SessionID, e.Err, e.SessionID)
}

func (e *StartCreateFailedError) Unwrap() error { return e.Err }
