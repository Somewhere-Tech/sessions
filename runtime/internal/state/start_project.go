package state

import (
	"fmt"
	"strings"
)

// ProjectStart derives the start receipt's phase, evidence and recovery from
// the session's own observed state and the first request's delivery. start
// carries the recorded operation ids; prompt is the first request's delivery
// and is ignored when no prompt operation was recorded. Nothing here reads or
// changes the session: a provider fault is reported exactly as recorded and
// never cleared, and no recovery ever resends or approves anything.
func ProjectStart(info SessionInfo, start StartReceipt, prompt *StartPrompt) StartReceipt {
	start.Prompt, start.BlockedBy, start.Recovery = nil, "", nil
	start.Phase, start.Evidence, start.EvidenceSource = "", "", ""
	if start.PromptOperationID != "" {
		if prompt == nil {
			prompt = &StartPrompt{Status: StartPromptNotSent, Retry: true}
		}
		copied := *prompt
		start.Prompt = &copied
	}
	if projectStartBlocked(info, &start) {
		return start
	}
	deliveredAt, delivered := projectStartPrompt(info, &start)
	if delivered {
		projectStartActivity(info, &start, deliveredAt)
	}
	return start
}

func projectStartBlocked(info SessionInfo, start *StartReceipt) bool {
	switch {
	case info.FailureKind != "":
		start.Phase, start.BlockedBy, start.EvidenceSource = StartPhaseBlocked, info.FailureKind, "provider-fault"
		start.Evidence = firstNonEmpty(info.FailureDetail, "the provider reported a failed turn")
		if info.FailureEvidence != "" {
			start.Evidence += " — provider said: " + info.FailureEvidence
		}
		start.Recovery = providerFaultRecovery(info)
	case info.PendingApproval != nil:
		start.Phase, start.BlockedBy, start.EvidenceSource = StartPhaseBlocked, "approval", "approval"
		start.Evidence = "waiting on approval: " + firstNonEmpty(info.PendingApproval.Summary, info.PendingApproval.Kind)
		start.Recovery = &StartRecovery{Action: StartRecoveryAnswer, Detail: fmt.Sprintf(
			"Review the request, then answer it with `sessions approve %s` or `sessions approve %s --deny`. Sessions never answers it for you.", info.ID, info.ID)}
	case info.IdleReason == IdleReasonNeedsInput && !info.Working && !info.Exited:
		start.Phase, start.BlockedBy, start.EvidenceSource = StartPhaseBlocked, IdleReasonNeedsInput, activitySource(info)
		start.Evidence = "the provider is waiting for an answer: " + firstNonEmpty(info.IdleDetail, "a question or prompt is open")
		start.Recovery = &StartRecovery{Action: StartRecoveryAnswer, Command: "sessions snap " + info.ID,
			Detail: "Read the open question and answer it in the session. Sessions never accepts a prompt by pressing Enter for you."}
	default:
		return false
	}
	return true
}

func providerFaultRecovery(info SessionInfo) *StartRecovery {
	provider := providerLabel(info)
	switch info.FailureKind {
	case "auth":
		command := ""
		detail := fmt.Sprintf("The %s account needs to be connected: its login is missing, expired or was refused. Connect the account in Sessions → Accounts", provider)
		if tool, ok := ProfileToolName(info.Tool); ok && info.Profile != "" {
			command = fmt.Sprintf("sessions accounts login %s --tool %s", info.Profile, tool)
			detail += " or run the command shown"
		} else if info.Kind == "" {
			detail += " or sign in inside the session's terminal"
		}
		return &StartRecovery{Action: StartRecoveryConnectAccount, Command: command,
			Detail: detail + ", then " + retryAdvice(info) + ". Sessions did not clear this error or resend anything."}
	case "provider-unavailable", "rate-limited":
		if info.Retry != nil {
			return &StartRecovery{Action: StartRecoveryWait, Command: "sessions wait " + info.ID, Detail: fmt.Sprintf(
				"%s did not complete the turn; Sessions retries it automatically (attempt %d of %d).", provider, info.Retry.Attempt, info.Retry.Max)}
		}
	}
	if structuredRuntime(info) {
		return &StartRecovery{Action: StartRecoveryRetryTurn, Command: "sessions retry " + info.ID,
			Detail: "Retry the failed turn once the cause is fixed; the failed request is kept, so nothing is sent twice."}
	}
	return &StartRecovery{Action: StartRecoveryInspect, Command: "sessions snap " + info.ID,
		Detail: "Read the provider's error in the session before sending the request again."}
}

func retryAdvice(info SessionInfo) string {
	if structuredRuntime(info) {
		return fmt.Sprintf("retry the failed turn with `sessions retry %s`", info.ID)
	}
	return "send the request again once the provider accepts it"
}

// projectStartPrompt reports when the first request is known to have arrived.
// Without a recorded prompt operation there is nothing to deliver, and the
// session's creation is the only boundary later activity is compared with.
func projectStartPrompt(info SessionInfo, start *StartReceipt) (int64, bool) {
	prompt := start.Prompt
	if prompt == nil {
		return info.CreatedAt, true
	}
	start.EvidenceSource = "delivery-receipt"
	switch prompt.Status {
	case StartPromptAccepted:
		return prompt.At, true
	case StartPromptSending:
		start.Phase, start.Evidence = StartPhaseCreated, "the first request is being delivered now"
		start.Recovery = &StartRecovery{Action: StartRecoveryWait, Command: "sessions send-status " + start.PromptOperationID,
			Detail: "Wait for the delivery receipt; do not send the request again while it is in flight."}
	case StartPromptNotSent:
		start.Phase = StartPhaseCreated
		start.Evidence = "the session was created; its first request has no delivery receipt, so nothing was sent"
		start.Recovery = sendPromptRecovery(info, start.PromptOperationID)
	case StartPromptNotDelivered:
		start.Phase = StartPhasePromptNotDelivered
		start.Evidence = "the first request was refused before it reached the provider: " + firstNonEmpty(prompt.Reason, "no reason recorded")
		start.Recovery = sendPromptRecovery(info, start.PromptOperationID)
		if !prompt.Retry {
			start.Recovery = inspectPromptRecovery(info)
		}
	default:
		// Unknown stays unknown. A later user turn in the transcript is not this
		// request: it can be anyone's message in a resumed or shared
		// conversation. Only the receipt itself -- reconciled from the runner's
		// own acknowledgment -- may settle it.
		start.Phase = StartPhasePromptUnknown
		start.Evidence = "Sessions cannot prove whether the first request arrived: " + firstNonEmpty(prompt.Reason, prompt.Status)
		start.Recovery = inspectPromptRecovery(info)
	}
	return 0, false
}

func sendPromptRecovery(info SessionInfo, promptOperationID string) *StartRecovery {
	if info.Exited {
		return &StartRecovery{Action: StartRecoveryStartNew,
			Detail: "The session ended before its first request reached the provider, so starting a new session cannot duplicate the work."}
	}
	return &StartRecovery{Action: StartRecoverySendPrompt,
		Command: fmt.Sprintf("sessions send %s --operation-id %s -- <first request>", info.ID, promptOperationID),
		Detail: "Nothing reached the provider. Send the same first request with this operation id, or re-run the original " +
			"`sessions new` command with the same --operation-id; Sessions delivers it at most once."}
}

func inspectPromptRecovery(info SessionInfo) *StartRecovery {
	return &StartRecovery{Action: StartRecoveryInspect, Command: fmt.Sprintf("sessions last %s --role user", info.ID),
		Detail: "Sessions will not resend the first request. Read the conversation first; send it again only if it is not there."}
}

func projectStartActivity(info SessionInfo, start *StartReceipt, deliveredAt int64) {
	switch {
	case info.Working && !info.Exited:
		start.Phase, start.EvidenceSource = StartPhaseWorking, activitySource(info)
		start.Evidence = "the provider reported an active turn"
		if start.EvidenceSource == "terminal" {
			start.Evidence = "the provider's terminal shows it working"
		}
	case info.IdleReason == IdleReasonCompleted && info.IdleSince != nil && *info.IdleSince >= deliveredAt:
		start.Phase, start.EvidenceSource = StartPhaseCompleted, activitySource(info)
		start.Evidence = "the provider finished its turn"
		if summary := strings.TrimSpace(info.LastSummary); summary != "" {
			start.Evidence += ": " + truncateStartEvidence(summary)
		}
	case start.Prompt == nil:
		start.Phase, start.EvidenceSource = StartPhaseCreated, "session"
		start.Evidence = "the session was created; no first request was sent through Sessions"
	default:
		start.Phase = StartPhasePromptDelivered
		start.Evidence = deliveredEvidence(start)
		start.Recovery = &StartRecovery{Action: StartRecoveryWait, Command: "sessions wait " + info.ID,
			Detail: "The request arrived; no provider turn has been observed yet."}
	}
	if info.Exited && start.Phase != StartPhaseCompleted {
		start.Recovery = &StartRecovery{Action: StartRecoveryInspect, Command: "sessions status " + info.ID,
			Detail: "The session's process ended before its work completed; inspect it before starting new work."}
	}
}

func deliveredEvidence(start *StartReceipt) string {
	switch start.Prompt.Acceptance {
	case "provider":
		return "the provider accepted the first request"
	case "transcript":
		return "the provider transcript records the first request"
	case "runner":
		return "the session runner accepted the first request; the provider has not reported a turn yet"
	}
	return "the first request was accepted"
}

func activitySource(info SessionInfo) string {
	if structuredRuntime(info) {
		return "provider-events"
	}
	return "terminal"
}

func structuredRuntime(info SessionInfo) bool {
	return info.MessageSubmit || info.Kind == KindClaudeStructured || info.Kind == KindCodexAppServer
}

func providerLabel(info SessionInfo) string {
	switch {
	case info.FailureProvider == "codex" || info.Tool == ToolCodex:
		return "Codex"
	case info.FailureProvider == "claude" || info.Tool == ToolClaude:
		return "Claude"
	}
	return "provider"
}

func truncateStartEvidence(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > 200 {
		return string(runes[:199]) + "…"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
