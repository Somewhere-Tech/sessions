package api

import (
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// semanticSubmitRefusal distinguishes provider terminal controls from an
// ordinary assistant question. Both are actionable needs-input states, but a
// semantic message sent to a provider menu can activate its selected choice.
func semanticSubmitRefusal(info state.SessionInfo) (string, bool) {
	if info.IdleReason != state.IdleReasonNeedsInput {
		return "", false
	}
	detail := strings.TrimSpace(info.IdleDetail)
	normalized := strings.ToLower(detail)
	providerControl := strings.Contains(normalized, "trust this folder") ||
		strings.Contains(normalized, "project you created or one you trust") ||
		strings.HasPrefix(normalized, "allow ") ||
		strings.HasPrefix(normalized, "approve ") ||
		strings.Contains(normalized, "press enter to confirm")
	if !providerControl {
		return "", false
	}
	if detail == "" {
		detail = "the provider is waiting for a terminal choice"
	}
	return "message not sent: " + detail + "; answer the provider control in Terminal view or with `sessions input`", true
}
