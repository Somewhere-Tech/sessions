// Package providerfault turns provider-specific outage prose into stable
// Sessions state. It deliberately classifies facts; retry policy belongs to
// the daemon layer that can account for session lifecycle and user intent.
package providerfault

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	KindUnavailable = "provider-unavailable"
	KindRateLimited = "rate-limited"
	KindAuth        = "auth"
	KindOther       = "other"
)

type Fault struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Status int    `json:"status,omitempty"`
}

var (
	statusRE      = regexp.MustCompile(`\b([45]\d\d)\b`)
	rateRE        = regexp.MustCompile(`(?i)\b(?:rate limit|too many requests|usage limit|quota)\b`)
	authRE        = regexp.MustCompile(`(?i)\b(?:not logged in|please run /login|invalid api key|unauthorized|authentication|auth error)\b`)
	unavailableRE = regexp.MustCompile(`(?i)\b(?:overloaded|service unavailable|server error|stream disconnected|reconnecting|connection (?:refused|reset|closed)|timed? out|timeout|ENOTFOUND|ECONNREFUSED|ECONNRESET|EAI_AGAIN|fetch failed|network)\b`)
	retryAfterRE  = regexp.MustCompile(`(?i)\b(?:try again in|retry after)\s+(\d+)\s*(ms|milliseconds?|s|secs?|seconds?|m|mins?|minutes?)\b`)
)

// Classify returns a stable fault for any failed provider turn. Detect is the
// matching form for terminal tails, where ordinary assistant prose must not be
// mistaken for a failure merely because unmatched failures classify as other.
func Classify(provider, text string, statusCode int) Fault {
	fault, matched := Detect(provider, text, statusCode)
	if matched {
		return fault
	}
	name := providerName(provider)
	detail := concise(text)
	if detail == "" {
		detail = "provider turn failed"
	}
	return Fault{Kind: KindOther, Detail: name + " turn failed: " + detail, Status: normalizedStatus(text, statusCode)}
}

func Detect(provider, text string, statusCode int) (Fault, bool) {
	status := normalizedStatus(text, statusCode)
	lower := strings.ToLower(text)
	name := providerName(provider)
	switch {
	case status == 401 || status == 403 || authRE.MatchString(text):
		return Fault{Kind: KindAuth, Detail: authDetail(name, lower, status), Status: status}, true
	case status == 429 || rateRE.MatchString(text):
		return Fault{Kind: KindRateLimited, Detail: statusDetail(name+" rate limit reached", status), Status: status}, true
	case status >= 500 && status <= 599 || unavailableRE.MatchString(text):
		return Fault{Kind: KindUnavailable, Detail: unavailableDetail(name, lower, status), Status: status}, true
	default:
		return Fault{}, false
	}
}

func HistoryEvent(provider string, fault Fault, at time.Time) (json.RawMessage, error) {
	value := map[string]any{
		"type": "system", "subtype": "provider_fault", "provider": canonicalProvider(provider),
		"kind": fault.Kind, "detail": fault.Detail, "timestamp": at.UTC().Format(time.RFC3339Nano),
	}
	if fault.Status != 0 {
		value["status"] = fault.Status
	}
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

func RetryHistoryEvent(attempt, maximum int, nextAt time.Time) (json.RawMessage, error) {
	value := map[string]any{
		"type": "system", "subtype": "provider_retry", "attempt": attempt,
		"max": maximum, "nextAt": nextAt.UnixMilli(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

// RetryAfter extracts the provider's requested delay from compact outage
// prose such as "try again in 42s". Policy decides whether to use it.
func RetryAfter(text string) time.Duration {
	match := retryAfterRE.FindStringSubmatch(text)
	if len(match) != 3 {
		return 0
	}
	var count int64
	if _, err := fmt.Sscanf(match[1], "%d", &count); err != nil || count <= 0 {
		return 0
	}
	unit := strings.ToLower(match[2])
	switch unit[0] {
	case 'm':
		if strings.HasPrefix(unit, "ms") || strings.HasPrefix(unit, "millisecond") {
			return time.Duration(count) * time.Millisecond
		}
		return time.Duration(count) * time.Minute
	default:
		return time.Duration(count) * time.Second
	}
}

func unavailableDetail(name, lower string, status int) string {
	switch {
	case strings.Contains(lower, "overload"):
		if name == "Claude" {
			return statusDetail("Claude API overloaded", status)
		}
		return qualifiedStatusDetail(name+" API unavailable", status, "overloaded")
	case strings.Contains(lower, "reconnecting"):
		return name + " connection interrupted (reconnecting)"
	case strings.Contains(lower, "stream disconnected"):
		return name + " stream disconnected"
	case strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout"):
		return name + " API request timed out"
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "econnrefused"):
		return name + " API connection failed (connection refused)"
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "econnreset"):
		return name + " API connection failed (connection reset)"
	case strings.Contains(lower, "connection closed"):
		return name + " API connection closed"
	case strings.Contains(lower, "enotfound") || strings.Contains(lower, "eai_again"):
		return name + " API host could not be resolved"
	case strings.Contains(lower, "network") || strings.Contains(lower, "fetch failed"):
		return name + " network unavailable"
	default:
		return statusDetail(name+" API unavailable", status)
	}
}

func authDetail(name, lower string, status int) string {
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "please run /login") {
		return name + " is not logged in"
	}
	if strings.Contains(lower, "invalid api key") {
		return name + " API key is invalid"
	}
	return statusDetail(name+" authentication failed", status)
}

func normalizedStatus(text string, explicit int) int {
	if explicit > 0 {
		return explicit
	}
	match := statusRE.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0
	}
	var status int
	_, _ = fmt.Sscanf(match[1], "%d", &status)
	return status
}

func statusDetail(detail string, status int) string {
	if status == 0 {
		return detail
	}
	return fmt.Sprintf("%s (%d)", detail, status)
}

func qualifiedStatusDetail(detail string, status int, qualifier string) string {
	if status == 0 {
		return detail + " (" + qualifier + ")"
	}
	return fmt.Sprintf("%s (%d, %s)", detail, status, qualifier)
}

func providerName(provider string) string {
	switch canonicalProvider(provider) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	default:
		return "Provider"
	}
}

func canonicalProvider(provider string) string {
	if strings.Contains(strings.ToLower(provider), "claude") {
		return "claude"
	}
	if strings.Contains(strings.ToLower(provider), "codex") {
		return "codex"
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

func concise(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > 160 {
		text = strings.TrimRightFunc(string(runes[:159]), unicode.IsSpace) + "…"
	}
	return text
}
