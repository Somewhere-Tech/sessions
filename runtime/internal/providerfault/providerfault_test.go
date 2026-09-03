package providerfault

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyObservedProviderFailures(t *testing.T) {
	tests := []struct {
		name, provider, text, kind, detail string
		status                             int
	}{
		{name: "claude overloaded", provider: "claude", text: "API Error: Repeated 529 Overloaded errors. The API is at capacity — this is usually temporary.", kind: KindUnavailable, detail: "Claude API overloaded (529)", status: 529},
		{name: "codex unavailable", provider: "codex", text: "unexpected status 503 Service Unavailable: The server is currently overloaded. Please try again later.", kind: KindUnavailable, detail: "Codex API unavailable (503, overloaded)", status: 503},
		{name: "reconnecting", provider: "codex", text: "Reconnecting... 1/5", kind: KindUnavailable, detail: "Codex connection interrupted (reconnecting)"},
		{name: "auth status", provider: "codex", text: "auth error: 401", kind: KindAuth, detail: "Codex authentication failed (401)", status: 401},
		{name: "login", provider: "claude", text: "Not logged in · Please run /login", kind: KindAuth, detail: "Claude is not logged in"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.provider, test.text, 0)
			if got.Kind != test.kind || got.Detail != test.detail || got.Status != test.status {
				t.Fatalf("Classify() = %#v, want kind=%q detail=%q status=%d", got, test.kind, test.detail, test.status)
			}
		})
	}
}

func TestClassifyStatusAndOtherFailures(t *testing.T) {
	if got := Classify("codex", "capacity exhausted", 429); got.Kind != KindRateLimited || got.Detail != "Codex rate limit reached (429)" {
		t.Fatalf("429 classification = %#v", got)
	}
	if got := Classify("claude", "subprocess broke", 0); got.Kind != KindOther || !strings.Contains(got.Detail, "subprocess broke") {
		t.Fatalf("other classification = %#v", got)
	}
	if _, matched := Detect("codex", "ordinary assistant text", 0); matched {
		t.Fatal("ordinary terminal text matched a provider fault")
	}
}

func TestHistoryEventUsesContractShape(t *testing.T) {
	fault := Fault{Kind: KindUnavailable, Detail: "Claude API overloaded (529)", Status: 529}
	raw, err := HistoryEvent("claude-code", fault, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"system"`, `"subtype":"provider_fault"`, `"provider":"claude"`, `"status":529`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("history event %s does not contain %s", raw, want)
		}
	}
}

func TestRetryAfterHint(t *testing.T) {
	for _, test := range []struct {
		text string
		want time.Duration
	}{
		{text: "rate limited; try again in 42s", want: 42 * time.Second},
		{text: "Retry after 2 minutes", want: 2 * time.Minute},
		{text: "try later", want: 0},
	} {
		if got := RetryAfter(test.text); got != test.want {
			t.Errorf("RetryAfter(%q) = %s, want %s", test.text, got, test.want)
		}
	}
}

func TestRetryHistoryEventUsesContractShape(t *testing.T) {
	raw, err := RetryHistoryEvent(2, 5, time.Unix(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"system"`, `"subtype":"provider_retry"`, `"attempt":2`, `"max":5`, `"nextAt":42000`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("retry history event %s does not contain %s", raw, want)
		}
	}
}
