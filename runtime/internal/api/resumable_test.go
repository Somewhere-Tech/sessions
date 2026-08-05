package api

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestMergeResumableConversationsGroupsContinuationRunsByProviderIdentity(t *testing.T) {
	const providerID = "11111111-1111-4111-8111-111111111111"
	got := mergeResumableConversations(
		[]watch.ResumableSession{{
			SessionID: providerID, Tool: "claude", Title: "Claude — first request",
			Cwd: "/workspace", ModifiedAt: 100,
		}},
		[]integrations.HistorySession{
			{
				ID: "lane-one", Name: "BOLO", Tool: "claude-code",
				ProviderSessionID: providerID, CWD: "/workspace",
				CreatedAt: 10, LastActivityAt: 120, ReopenedAs: "lane-two",
			},
			{
				ID: "lane-two", Name: "BOLO", Tool: "claude-code",
				ProviderSessionID: providerID, CWD: "/workspace",
				CreatedAt: 20, LastActivityAt: 140, ResumedFrom: "lane-one",
			},
			{
				ID: "external:claude:" + providerID, Name: "BOLO", Tool: "claude",
				ProviderSessionID: providerID, CWD: "/workspace",
				LastActivityAt: 140, External: true,
			},
		},
	)
	if len(got) != 1 {
		t.Fatalf("conversations = %#v, want one provider-neutral row", got)
	}
	if got[0].SessionID != providerID || got[0].Title != "BOLO" || got[0].ModifiedAt != 140 {
		t.Fatalf("conversation = %#v", got[0])
	}
	if len(got[0].Runs) != 2 ||
		got[0].Runs[0].SessionID != "lane-one" ||
		got[0].Runs[1].SessionID != "lane-two" {
		t.Fatalf("continuation runs = %#v", got[0].Runs)
	}
	if got[0].External {
		t.Fatalf("linked Sessions conversation marked external: %#v", got[0])
	}
}

func TestMergeResumableConversationsMarksProviderOnlyHistoryExternal(t *testing.T) {
	const providerID = "33333333-3333-4333-8333-333333333333"
	got := mergeResumableConversations([]watch.ResumableSession{{
		SessionID: providerID, Tool: "codex", Title: "Native Codex chat",
		Cwd: "/workspace", ModifiedAt: 100,
	}}, nil)
	if len(got) != 1 || !got[0].External {
		t.Fatalf("provider-only conversation = %#v, want external marker", got)
	}
}

func TestMergeResumableConversationsIncludesClaudePromptIndexOnlyHistory(t *testing.T) {
	const providerID = "22222222-2222-4222-8222-222222222222"
	const historyID = "provider-history:claude:" + providerID
	got := mergeResumableConversations(nil, []integrations.HistorySession{{
		ID: historyID, Name: "Archived BOLO planning", Tool: "claude",
		ProviderSessionID: providerID, CWD: "/workspace",
		LastActivityAt: 123, External: true, PromptHistoryOnly: true,
	}})
	if len(got) != 1 {
		t.Fatalf("conversations = %#v, want one prompt-index row", got)
	}
	if got[0].HistoryID != historyID || !got[0].PromptHistoryOnly ||
		!got[0].External || got[0].Origin != "Claude prompt index" ||
		got[0].Title != "Archived BOLO planning" {
		t.Fatalf("prompt-index conversation = %#v", got[0])
	}
}

func TestMergeResumableConversationsIncludesTranscriptRecoveryWithoutProviderHandle(t *testing.T) {
	got := mergeResumableConversations(nil, []integrations.HistorySession{{
		ID: "db-final-review-sol-id", Name: "db-final-review-sol", Tool: "codex",
		CWD: "/workspace", CreatedAt: 100, LastActivityAt: 123,
		ConversationAvailable: true,
	}})
	if len(got) != 1 || !got[0].TranscriptRecovery ||
		got[0].HistoryID != "db-final-review-sol-id" || got[0].Title != "db-final-review-sol" ||
		got[0].Origin != "Sessions recovery" {
		t.Fatalf("transcript recovery conversation = %#v", got)
	}
}
