package state

import (
	"strings"
	"testing"
)

const (
	projectStartOp  = "5b000000-0000-4000-8000-000000000001"
	projectPromptOp = "5b000000-0000-4000-8000-000000000002"
)

func int64Pointer(value int64) *int64 { return &value }

func projectStartCase(info SessionInfo, prompt *StartPrompt) StartReceipt {
	if info.ID == "" {
		info.ID = "session-1"
	}
	if info.CreatedAt == 0 {
		info.CreatedAt = 1_000
	}
	return ProjectStart(info, StartReceipt{OperationID: projectStartOp, PromptOperationID: projectPromptOp}, prompt)
}

// Each step of a delegated start is claimed only from its own evidence.
func TestProjectStartDistinguishesEveryStep(t *testing.T) {
	accepted := &StartPrompt{Status: StartPromptAccepted, Acceptance: "runner", At: 2_000}
	cases := []struct {
		name       string
		info       SessionInfo
		prompt     *StartPrompt
		phase      string
		recovery   string
		evidenceIn string
	}{
		{name: "created, first request never submitted", prompt: &StartPrompt{Status: StartPromptNotSent, Retry: true},
			phase: StartPhaseCreated, recovery: StartRecoverySendPrompt, evidenceIn: "nothing was sent"},
		{name: "launching waits instead of sending or recreating", info: SessionInfo{Launching: true},
			prompt: &StartPrompt{Status: StartPromptNotSent, Retry: true}, phase: StartPhaseCreated,
			recovery: StartRecoveryWait, evidenceIn: "readiness has not been confirmed"},
		{name: "delivery in flight", prompt: &StartPrompt{Status: StartPromptSending},
			phase: StartPhaseCreated, recovery: StartRecoveryWait},
		{name: "unreachable runner is blocked, not idle or ready", info: SessionInfo{Unreachable: true, LostReason: "runner startup was never confirmed"},
			prompt: &StartPrompt{Status: StartPromptUnknown}, phase: StartPhaseBlocked, recovery: StartRecoveryInspect, evidenceIn: "startup was never confirmed"},
		{name: "refused before the provider, safe to send again", prompt: &StartPrompt{Status: StartPromptNotDelivered, Retry: true, Reason: "turn active"},
			phase: StartPhasePromptNotDelivered, recovery: StartRecoverySendPrompt, evidenceIn: "turn active"},
		{name: "refused without proof nothing arrived", prompt: &StartPrompt{Status: StartPromptNotDelivered, Reason: "odd"},
			phase: StartPhasePromptNotDelivered, recovery: StartRecoveryInspect},
		{name: "ambiguous timeout is never resent", prompt: &StartPrompt{Status: StartPromptUnknown, Reason: "timeout"},
			phase: StartPhasePromptUnknown, recovery: StartRecoveryInspect, evidenceIn: "cannot prove"},
		{name: "unreadable receipt is not mistaken for not-sent", prompt: &StartPrompt{Status: StartPromptUnreadable, Reason: "permission denied"},
			phase: StartPhasePromptUnknown, recovery: StartRecoveryInspect, evidenceIn: "permission denied"},
		{name: "runner acceptance is delivery, not work", prompt: accepted,
			phase: StartPhasePromptDelivered, recovery: StartRecoveryWait, evidenceIn: "runner accepted"},
		{name: "structured working is provider evidence", info: SessionInfo{Working: true, Kind: KindClaudeStructured}, prompt: accepted,
			phase: StartPhaseWorking, evidenceIn: "provider reported"},
		{name: "terminal working says it is terminal evidence", info: SessionInfo{Working: true}, prompt: accepted,
			phase: StartPhaseWorking, evidenceIn: "terminal"},
		{name: "completion after delivery", info: SessionInfo{IdleReason: IdleReasonCompleted, IdleSince: int64Pointer(3_000), LastSummary: "done: 3 files"}, prompt: accepted,
			phase: StartPhaseCompleted, evidenceIn: "done: 3 files"},
		{name: "completion from before the request is not this task", info: SessionInfo{IdleReason: IdleReasonCompleted, IdleSince: int64Pointer(1_500)}, prompt: accepted,
			phase: StartPhasePromptDelivered},
		// A later user turn can be an unrelated message in a resumed or shared
		// conversation; it never settles an uncertain first request.
		{name: "an unrelated later user turn does not settle an uncertain request", info: SessionInfo{LastUserMessageAt: int64Pointer(5_000), Working: true}, prompt: &StartPrompt{Status: StartPromptUnknown},
			phase: StartPhasePromptUnknown, recovery: StartRecoveryInspect},
		{name: "an unrelated later user turn does not settle a text-only request", info: SessionInfo{LastUserMessageAt: int64Pointer(5_000), IdleReason: IdleReasonCompleted, IdleSince: int64Pointer(6_000)}, prompt: &StartPrompt{Status: StartPromptTextOnly},
			phase: StartPhasePromptUnknown, recovery: StartRecoveryInspect},
		{name: "ended before delivery starts a new session", info: SessionInfo{Exited: true}, prompt: &StartPrompt{Status: StartPromptNotSent, Retry: true},
			phase: StartPhaseCreated, recovery: StartRecoveryStartNew},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := projectStartCase(test.info, test.prompt)
			if got.Phase != test.phase {
				t.Fatalf("phase = %q, want %q (%#v)", got.Phase, test.phase, got)
			}
			if test.recovery != "" && (got.Recovery == nil || got.Recovery.Action != test.recovery) {
				t.Fatalf("recovery = %#v, want %q", got.Recovery, test.recovery)
			}
			if !strings.Contains(got.Evidence, test.evidenceIn) {
				t.Fatalf("evidence = %q, want it to mention %q", got.Evidence, test.evidenceIn)
			}
			if got.EvidenceSource == "" {
				t.Fatalf("evidence source missing: %#v", got)
			}
		})
	}
}

// Authentication is a blocked start with a named safe action, never idle, and
// the fault is reported exactly as recorded.
func TestProjectStartBlockedAuthenticationNamesConnectAccount(t *testing.T) {
	info := SessionInfo{
		ID: "session-1", Tool: ToolClaude, Kind: KindClaudeStructured, Profile: "work", MessageSubmit: true,
		IdleReason: IdleReasonFailed, FailureKind: "auth", FailureDetail: "Claude is not logged in",
		FailureEvidence: "Please run /login", FailureProvider: "claude",
	}
	got := projectStartCase(info, &StartPrompt{Status: StartPromptAccepted, Acceptance: "runner", At: 2_000})
	if got.Phase != StartPhaseBlocked || got.BlockedBy != "auth" || got.EvidenceSource != "provider-fault" {
		t.Fatalf("auth start = %#v", got)
	}
	if got.Recovery == nil || got.Recovery.Action != StartRecoveryConnectAccount ||
		got.Recovery.Command != "sessions accounts login work --tool claude" ||
		!strings.Contains(got.Recovery.Detail, "Accounts") || !strings.Contains(got.Recovery.Detail, "sessions retry session-1") {
		t.Fatalf("auth recovery = %#v", got.Recovery)
	}
	if !strings.Contains(got.Evidence, "Please run /login") {
		t.Fatalf("auth evidence dropped the provider's own line: %q", got.Evidence)
	}
	if got.Prompt == nil || got.Prompt.Status != StartPromptAccepted {
		t.Fatalf("blocked start hid the prompt delivery: %#v", got.Prompt)
	}
}

func TestProjectStartApprovalIsAnsweredNeverGranted(t *testing.T) {
	info := SessionInfo{ID: "session-1", PendingApproval: &ApprovalPrompt{ID: "a", Kind: "command", Summary: "rm -rf build"}}
	got := projectStartCase(info, &StartPrompt{Status: StartPromptAccepted, At: 2_000})
	if got.Phase != StartPhaseBlocked || got.BlockedBy != "approval" || got.Recovery.Action != StartRecoveryAnswer || got.Recovery.Command != "" {
		t.Fatalf("approval start = %#v", got)
	}
}

func TestProjectStartWithoutFirstRequest(t *testing.T) {
	got := ProjectStart(SessionInfo{ID: "s", CreatedAt: 1}, StartReceipt{OperationID: projectStartOp}, nil)
	if got.Phase != StartPhaseCreated || got.Prompt != nil || got.Recovery != nil {
		t.Fatalf("no-prompt start = %#v", got)
	}
	working := ProjectStart(SessionInfo{ID: "s", CreatedAt: 1, Working: true}, StartReceipt{OperationID: projectStartOp}, nil)
	if working.Phase != StartPhaseWorking {
		t.Fatalf("no-prompt working start = %#v", working)
	}
}

func TestValidateStartOperationIDs(t *testing.T) {
	if err := ValidateStartOperationIDs(projectStartOp, projectPromptOp); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"x", ""}, {"", "X"}, {projectStartOp, projectStartOp}, {strings.ToUpper(projectStartOp), ""}} {
		if ValidateStartOperationIDs(pair[0], pair[1]) == nil {
			t.Fatalf("accepted %q/%q", pair[0], pair[1])
		}
	}
}
