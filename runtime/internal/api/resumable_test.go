package api

import (
	"net/http"
	"os"
	"strings"
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

// The Resume list is built from the same history the torn-record policy
// degrades. It used to forward only the rows and drop the counters, so a Resume
// dialog missing a conversation because its transcript could not be read was
// indistinguishable from one where that conversation never existed.
func TestResumableConversationsReportTheHistoryDegradationBehindThem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unreadable-file permissions this test relies on")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)

	clean := serve(t, daemon.handler, http.MethodGet, "/api/resumable-conversations", nil, "127.0.0.1:4321", nil)
	if clean.Code != http.StatusOK {
		t.Fatalf("resumable status=%d body=%s", clean.Code, clean.Body.String())
	}
	// A clean listing keeps the documented shape: an absent counter still means
	// nothing was lost.
	if body := clean.Body.String(); strings.Contains(body, "unreadable_sessions") || strings.Contains(body, "skipped_records") {
		t.Fatalf("clean resumable listing must omit the counters: %s", body)
	}

	unreadablePath := writeClaudeHistoryFixture(t, daemon, home,
		"cccccccc-8888-4888-8888-cccccccccccc", "unreadable recall", claudeTranscriptLines("lost question"))
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadablePath, 0o600) })

	degraded := serve(t, daemon.handler, http.MethodGet, "/api/resumable-conversations", nil, "127.0.0.1:4321", nil)
	if degraded.Code != http.StatusOK {
		t.Fatalf("degraded resumable status=%d body=%s", degraded.Code, degraded.Body.String())
	}
	var listing resumableListing
	decodeBody(t, degraded, &listing)
	if listing.UnreadableSessions != 1 {
		t.Fatalf("unreadable_sessions = %d, want 1: %s", listing.UnreadableSessions, degraded.Body.String())
	}
	t.Logf("resumable listing: sessions=%d unreadable_sessions=%d skipped_records=%d",
		len(listing.Sessions), listing.UnreadableSessions, listing.SkippedRecords)
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
