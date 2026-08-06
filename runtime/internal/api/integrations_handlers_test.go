package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestHistoryRoutesExposeStableListTranscriptTextAndRawShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	id := "11111111-2222-4333-8444-555555555555"
	cwd := filepath.Join(daemon.root, "fixture-worktree")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.July, 16, 17, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(daemon.config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(daemon.config.RunnerStateDir, id+".json")
	if err := state.WriteMetadata(metadataPath, state.Metadata{
		ID: id, Name: "fixture recall", Cmd: "claude", Args: []string{"--session-id", id},
		Cwd: cwd, Cols: 120, Rows: 40, CreatedAt: created.UnixMilli(), PID: 4242,
		SockPath: filepath.Join(daemon.config.RunnerStateDir, id+".sock"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, created, created); err != nil {
		t.Fatal(err)
	}
	conversation := []byte(strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-07-16T17:01:00Z","entrypoint":"cli","version":"2.1.220","message":{"role":"user","content":"Recall this fixture"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-07-16T17:01:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Fixture remembered."}]}}`,
		`{"type":"user","uuid":"tool1","timestamp":"2026-07-16T17:01:03Z","message":{"role":"user","content":[{"type":"tool_result","content":"not a conversation turn"}]}}`,
	}, "\n") + "\n")
	conversationPath := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conversationPath, conversation, 0o600); err != nil {
		t.Fatal(err)
	}
	modified := created.Add(2 * time.Minute)
	if err := os.Chtimes(conversationPath, modified, modified); err != nil {
		t.Fatal(err)
	}

	unauthorized := serve(t, daemon.handler, http.MethodGet, "/api/history", nil, "198.51.100.20:4321", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	list := serve(t, daemon.handler, http.MethodGet, "/api/history", nil, "127.0.0.1:4321", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var history integrations.HistoryResponse
	decodeBody(t, list, &history)
	if history.SchemaVersion != integrations.SchemaVersion || len(history.Sessions) != 1 {
		t.Fatalf("history = %#v", history)
	}
	listed := history.Sessions[0]
	// The conversation's own last record, not the file's modification time.
	// mtime here is two minutes past the last record precisely so the two
	// cannot be confused; see TestHistoryDatesConversationsByTheirOwnRecords.
	lastRecord := time.Date(2026, time.July, 16, 17, 1, 3, 0, time.UTC).UnixMilli()
	if listed.ID != id || listed.Name != "fixture recall" || listed.Tool != "claude" || listed.CWD != cwd ||
		listed.Machine == "" || listed.CreatedAt != created.UnixMilli() || listed.LastActivityAt != lastRecord ||
		listed.ConversationUpdatedAt != lastRecord || listed.ConversationUpdatedApproximate ||
		listed.MessageCount != 2 || !listed.ConversationAvailable {
		t.Fatalf("listed session = %#v (mtime was %d)", listed, modified.UnixMilli())
	}
	if listed.Surface == nil || listed.Surface.Kind != watch.SurfaceClaudeCLI {
		t.Fatalf("listed surface = %#v", listed.Surface)
	}

	transcriptResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"?format=json", nil, "127.0.0.1:4321", nil)
	if transcriptResponse.Code != http.StatusOK {
		t.Fatalf("transcript status=%d body=%s", transcriptResponse.Code, transcriptResponse.Body.String())
	}
	var transcript integrations.TranscriptResponse
	decodeBody(t, transcriptResponse, &transcript)
	if transcript.SchemaVersion != integrations.SchemaVersion || transcript.Session.ID != id || len(transcript.Messages) != 2 {
		t.Fatalf("transcript = %#v", transcript)
	}
	if transcript.Messages[0].Role != "user" || transcript.Messages[0].Text != "Recall this fixture" ||
		transcript.Messages[0].Timestamp == nil || *transcript.Messages[0].Timestamp != "2026-07-16T17:01:00Z" ||
		transcript.Messages[1].Role != "assistant" || transcript.Messages[1].Text != "Fixture remembered." {
		t.Fatalf("messages = %#v", transcript.Messages)
	}
	previewResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/preview?format=json", nil, "127.0.0.1:4321", nil)
	decodeBody(t, previewResponse, &transcript)
	if previewResponse.Code != http.StatusOK || transcript.Truncated || len(transcript.Messages) != 2 {
		t.Fatalf("preview status=%d transcript=%#v", previewResponse.Code, transcript)
	}
	windowResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/window?format=json&start=1&end=2", nil, "127.0.0.1:4321", nil)
	decodeBody(t, windowResponse, &transcript)
	if windowResponse.Code != http.StatusOK || !transcript.Truncated || transcript.Session.MessageCount != 2 ||
		len(transcript.Messages) != 1 || transcript.Messages[0].Index != 1 ||
		transcript.Messages[0].Text != "Fixture remembered." {
		t.Fatalf("window status=%d transcript=%#v", windowResponse.Code, transcript)
	}
	verifiedWindow := serve(t, daemon.handler, http.MethodGet,
		"/api/history/"+id+"/window?start=0&end=2&anchor=0&message_id="+transcriptResponseMessageID(t, transcriptResponse.Body.Bytes(), 0),
		nil, "127.0.0.1:4321", nil)
	if verifiedWindow.Code != http.StatusOK {
		t.Fatalf("verified window status=%d body=%s", verifiedWindow.Code, verifiedWindow.Body.String())
	}
	staleWindow := serve(t, daemon.handler, http.MethodGet,
		"/api/history/"+id+"/window?start=0&end=2&anchor=0&message_id=stale-bookmark",
		nil, "127.0.0.1:4321", nil)
	if staleWindow.Code != http.StatusConflict {
		t.Fatalf("stale window status=%d body=%s", staleWindow.Code, staleWindow.Body.String())
	}
	invalidWindow := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/window?start=2&end=1", nil, "127.0.0.1:4321", nil)
	if invalidWindow.Code != http.StatusBadRequest {
		t.Fatalf("invalid window status=%d body=%s", invalidWindow.Code, invalidWindow.Body.String())
	}
	overlargeWindow := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/window?start=0&end=501", nil, "127.0.0.1:4321", nil)
	if overlargeWindow.Code != http.StatusBadRequest {
		t.Fatalf("overlarge window status=%d body=%s", overlargeWindow.Code, overlargeWindow.Body.String())
	}
	invalidView := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/everything", nil, "127.0.0.1:4321", nil)
	if invalidView.Code != http.StatusNotFound {
		t.Fatalf("invalid view status=%d body=%s", invalidView.Code, invalidView.Body.String())
	}

	textResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"?format=text", nil, "127.0.0.1:4321", nil)
	if textResponse.Code != http.StatusOK || textResponse.Header().Get("X-Sessions-Schema-Version") != "1" ||
		textResponse.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		textResponse.Body.String() != "[user 2026-07-16T17:01:00Z]\nRecall this fixture\n\n[assistant 2026-07-16T17:01:02Z]\nFixture remembered.\n" {
		t.Fatalf("text status=%d headers=%v body=%q", textResponse.Code, textResponse.Header(), textResponse.Body.String())
	}

	rawResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/raw", nil, "127.0.0.1:4321", nil)
	if rawResponse.Code != http.StatusOK || rawResponse.Header().Get("Content-Type") != "application/octet-stream" ||
		!bytes.Equal(rawResponse.Body.Bytes(), conversation) {
		t.Fatalf("raw status=%d type=%q body=%q", rawResponse.Code, rawResponse.Header().Get("Content-Type"), rawResponse.Body.Bytes())
	}
	sourceResponse := serve(t, daemon.handler, http.MethodGet, "/api/history/"+id+"/source", nil, "127.0.0.1:4321", nil)
	if sourceResponse.Code != http.StatusOK {
		t.Fatalf("source status=%d body=%s", sourceResponse.Code, sourceResponse.Body.String())
	}
	var source integrations.HistorySource
	decodeBody(t, sourceResponse, &source)
	if source.Session.ID != id || source.SourcePath != conversationPath ||
		source.SourceKind != "provider-jsonl" || !source.RawAvailable ||
		!source.TextAvailable || source.RawBytes != int64(len(conversation)) {
		t.Fatalf("source = %#v", source)
	}
}

// writeClaudeHistoryFixture registers a managed Claude session and, when
// conversation is non-nil, the provider transcript it resolves to.
func writeClaudeHistoryFixture(t *testing.T, daemon testDaemon, home, id, name string, conversation []byte) string {
	t.Helper()
	cwd := filepath.Join(daemon.root, "worktree-"+id)
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(daemon.config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.August, 5, 9, 0, 0, 0, time.UTC)
	metadataPath := filepath.Join(daemon.config.RunnerStateDir, id+".json")
	if err := state.WriteMetadata(metadataPath, state.Metadata{
		ID: id, Name: name, Cmd: "claude", Args: []string{"--session-id", id},
		Cwd: cwd, Cols: 120, Rows: 40, CreatedAt: created.UnixMilli(), PID: 4242,
		SockPath: filepath.Join(daemon.config.RunnerStateDir, id+".sock"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, created, created); err != nil {
		t.Fatal(err)
	}
	if conversation == nil {
		return ""
	}
	conversationPath := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conversationPath, conversation, 0o600); err != nil {
		t.Fatal(err)
	}
	return conversationPath
}

func claudeTranscriptLines(texts ...string) []byte {
	lines := make([]string, 0, len(texts))
	for index, text := range texts {
		lines = append(lines, fmt.Sprintf(
			`{"type":"user","uuid":"u%d","timestamp":"2026-08-05T09:0%d:00Z","message":{"role":"user","content":%q}}`,
			index, index, text))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// The summary view is the cheap view, so it is the one a UI or an agent polls.
// It used to build its body by hand and always report zero torn records and zero
// unreadable sessions, which made a degraded history indistinguishable from a
// clean one on exactly the route most likely to be watched.
func TestHistorySummaryReportsDegradationAlongsideTheFullListing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unreadable-file permissions this test relies on")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	healthyID := "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	tornID := "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	unreadableID := "cccccccc-3333-4333-8333-cccccccccccc"
	writeClaudeHistoryFixture(t, daemon, home, healthyID, "healthy recall", claudeTranscriptLines("healthy question"))
	// A valid record, then invalid UTF-8, then a record a power cut truncated
	// mid-line with no trailing newline: two torn records, one usable message.
	torn := append(claudeTranscriptLines("torn question"),
		[]byte("\xff\xfe\x00 not utf-8 and not json\n"+
			`{"type":"user","uuid":"u9","timestamp":"2026-08-05T09:09:00Z","message":{"role":"user","content":"cut`)...)
	writeClaudeHistoryFixture(t, daemon, home, tornID, "torn recall", torn)
	unreadablePath := writeClaudeHistoryFixture(t, daemon, home, unreadableID, "unreadable recall",
		claudeTranscriptLines("lost question"))
	// Stat still succeeds; opening the file does not, the way a stale network
	// mount or a revoked ACL behaves.
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadablePath, 0o600) })

	full := serve(t, daemon.handler, http.MethodGet, "/api/history", nil, "127.0.0.1:4321", nil)
	if full.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", full.Code, full.Body.String())
	}
	var listing historyListResponse
	decodeBody(t, full, &listing)
	if listing.TranscriptsUnread {
		t.Fatalf("the full listing reads every transcript and must not claim otherwise: %s", full.Body.String())
	}
	byID := historySessionsByID(listing.Sessions)
	if len(byID) != 3 {
		t.Fatalf("history = %s", full.Body.String())
	}
	if byID[tornID].SkippedRecords != 2 || byID[tornID].Unreadable || byID[tornID].MessageCount != 1 {
		t.Fatalf("torn session = %#v", byID[tornID])
	}
	if !byID[unreadableID].Unreadable || byID[unreadableID].UnreadableReason == "" {
		t.Fatalf("unreadable session = %#v", byID[unreadableID])
	}
	if listing.SkippedRecords != 2 || listing.UnreadableSessions != 1 {
		t.Fatalf("full listing aggregates: skipped=%d unreadable_sessions=%d body=%s",
			listing.SkippedRecords, listing.UnreadableSessions, full.Body.String())
	}

	summary := serve(t, daemon.handler, http.MethodGet, "/api/history?summary=true", nil, "127.0.0.1:4321", nil)
	if summary.Code != http.StatusOK {
		t.Fatalf("summary status=%d body=%s", summary.Code, summary.Body.String())
	}
	var summarized historyListResponse
	decodeBody(t, summary, &summarized)
	if len(summarized.Sessions) != len(listing.Sessions) {
		t.Fatalf("summary listed %d sessions, full listing listed %d", len(summarized.Sessions), len(listing.Sessions))
	}
	// The summary view resolves and stats every source but never opens one, so
	// it cannot count torn records. Saying so is what keeps it honest: a zero
	// counter here means "not counted", not "nothing was lost".
	if !summarized.TranscriptsUnread {
		t.Fatalf("summary must declare that it did not read the transcripts it listed: %s", summary.Body.String())
	}
	if !strings.Contains(summary.Body.String(), `"transcripts_unread":true`) {
		t.Fatalf("summary body must carry transcripts_unread: %s", summary.Body.String())
	}
	// Whatever degradation the cheap pass did observe must be aggregated the
	// same way the full listing aggregates it, never dropped on the floor.
	expected := historySummaryListing(summarized.Sessions)
	if summarized.UnreadableSessions != expected.UnreadableSessions || summarized.SkippedRecords != expected.SkippedRecords {
		t.Fatalf("summary aggregates disagree with its own rows: response(unreadable=%d skipped=%d) rows(unreadable=%d skipped=%d)",
			summarized.UnreadableSessions, summarized.SkippedRecords,
			expected.UnreadableSessions, expected.SkippedRecords)
	}
	t.Logf("full: unreadable_sessions=%d skipped_records=%d; summary: unreadable_sessions=%d skipped_records=%d transcripts_unread=%v",
		listing.UnreadableSessions, listing.SkippedRecords,
		summarized.UnreadableSessions, summarized.SkippedRecords, summarized.TranscriptsUnread)
}

// The aggregation itself, pinned without a filesystem: the summary view counts
// every degraded row it was handed.
func TestHistorySummaryListingAggregatesEveryDegradedRow(t *testing.T) {
	listing := historySummaryListing([]integrations.HistorySession{
		{ID: "clean"},
		{ID: "torn", SkippedRecords: 3},
		{ID: "unreadable", Unreadable: true, UnreadableReason: "transcript could not be read"},
		{ID: "both", Unreadable: true, SkippedRecords: 4},
	})
	if listing.SchemaVersion != integrations.SchemaVersion || len(listing.Sessions) != 4 {
		t.Fatalf("listing = %#v", listing)
	}
	if listing.UnreadableSessions != 2 || listing.SkippedRecords != 7 || !listing.TranscriptsUnread {
		t.Fatalf("aggregates = unreadable %d, skipped %d, transcripts_unread %v",
			listing.UnreadableSessions, listing.SkippedRecords, listing.TranscriptsUnread)
	}
	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"unreadable_sessions":2`, `"skipped_records":7`, `"transcripts_unread":true`, `"schemaVersion":1`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("summary body %s is missing %s", encoded, want)
		}
	}
	// A clean listing stays byte-identical to the documented shape: an absent
	// counter still means nothing was lost.
	clean, err := json.Marshal(historyListResponse{HistoryResponse: integrations.HistoryResponse{
		SchemaVersion: integrations.SchemaVersion, Sessions: []integrations.HistorySession{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if string(clean) != `{"schemaVersion":1,"sessions":[]}` {
		t.Fatalf("clean listing = %s", clean)
	}
}

// `source_kind` is the only way a consumer can tell where a conversation came
// from, and `sessions-mirror` specifically means the provider deleted its
// transcript and this content is Sessions' own copy — native `claude --resume`
// will not work for that session. The handler must pass the store's value
// through untouched for every kind.
func TestHistorySourceReportsWhereEachConversationCameFrom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	providerID := "dddddddd-4444-4444-8444-dddddddddddd"
	mirrorID := "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	missingID := "ffffffff-6666-4666-8666-ffffffffffff"
	archivedID := "99999999-7777-4777-8777-999999999999"

	providerPath := writeClaudeHistoryFixture(t, daemon, home, providerID, "provider recall",
		claudeTranscriptLines("provider question"))
	// A session Sessions kept a copy of: no provider file resolves, only the
	// mirror beside the runner state.
	writeClaudeHistoryFixture(t, daemon, home, mirrorID, "mirrored recall", nil)
	mirrorPath := watch.TranscriptMirrorPath(daemon.config.RunnerStateDir, mirrorID)
	if err := os.WriteFile(mirrorPath, claudeTranscriptLines("mirrored question"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaudeHistoryFixture(t, daemon, home, missingID, "vanished recall", nil)
	archive := fmt.Sprintf(
		`{"display":"archived question","project":%q,"sessionId":%q,"timestamp":1785920400000}`+"\n",
		daemon.root, archivedID)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "history.jsonl"), []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, expected := range []struct {
		id            string
		kind          string
		path          string
		rawAvailable  bool
		textAvailable bool
	}{
		{id: providerID, kind: "provider-jsonl", path: providerPath, rawAvailable: true, textAvailable: true},
		{id: mirrorID, kind: "sessions-mirror", path: mirrorPath, rawAvailable: true, textAvailable: true},
		{id: missingID, kind: "missing"},
		{id: "provider-history:claude:" + archivedID, kind: "prompt-index", textAvailable: true},
	} {
		response := serve(t, daemon.handler, http.MethodGet, "/api/history/"+expected.id+"/source", nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("source(%s) status=%d body=%s", expected.id, response.Code, response.Body.String())
		}
		var source integrations.HistorySource
		decodeBody(t, response, &source)
		if source.SchemaVersion != integrations.SchemaVersion || source.Session.ID != expected.id {
			t.Fatalf("source(%s) = %#v", expected.id, source)
		}
		if source.SourceKind != expected.kind {
			t.Fatalf("source_kind(%s) = %q, want %q", expected.id, source.SourceKind, expected.kind)
		}
		if expected.path != "" && source.SourcePath != expected.path {
			t.Fatalf("source_path(%s) = %q, want %q", expected.id, source.SourcePath, expected.path)
		}
		if source.RawAvailable != expected.rawAvailable || source.TextAvailable != expected.textAvailable {
			t.Fatalf("source(%s) raw=%v text=%v, want raw=%v text=%v",
				expected.id, source.RawAvailable, source.TextAvailable, expected.rawAvailable, expected.textAvailable)
		}
	}

	// The mirrored session must really be served from Sessions' copy, which is
	// what makes the `sessions-mirror` label load-bearing rather than cosmetic.
	transcript := serve(t, daemon.handler, http.MethodGet, "/api/history/"+mirrorID+"?format=json", nil, "127.0.0.1:4321", nil)
	var decoded integrations.TranscriptResponse
	decodeBody(t, transcript, &decoded)
	if transcript.Code != http.StatusOK || len(decoded.Messages) != 1 || decoded.Messages[0].Text != "mirrored question" {
		t.Fatalf("mirrored transcript status=%d body=%s", transcript.Code, transcript.Body.String())
	}
	unknown := serve(t, daemon.handler, http.MethodGet, "/api/history/not-a-session/source", nil, "127.0.0.1:4321", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown source status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func historySessionsByID(sessions []integrations.HistorySession) map[string]integrations.HistorySession {
	byID := make(map[string]integrations.HistorySession, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	return byID
}

func transcriptResponseMessageID(t *testing.T, encoded []byte, index int) string {
	t.Helper()
	var transcript integrations.TranscriptResponse
	if err := json.Unmarshal(encoded, &transcript); err != nil {
		t.Fatal(err)
	}
	if index >= len(transcript.Messages) || transcript.Messages[index].ID == "" {
		t.Fatalf("transcript message id missing: %#v", transcript.Messages)
	}
	return transcript.Messages[index].ID
}

func TestErrorsRouteReturnsDurablePagingFeed(t *testing.T) {
	daemon := newTestDaemon(t)
	first, err := daemon.handler.integrationEndpoints.Emit(integrations.ErrorInput{
		TS: "2026-07-16T18:00:00Z", Kind: "dependency_error", SessionID: "session-a",
		Summary: "dependency missing", Detail: "fixture dependency was unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := daemon.handler.integrationEndpoints.Emit(integrations.ErrorInput{
		TS: "2026-07-16T18:00:01Z", Kind: "daemon_error",
		Summary: "fixture caught error", Detail: "synthetic detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("emitted seqs = %d, %d", first.Seq, second.Seq)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/api/errors", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("feed status=%d body=%s", response.Code, response.Body.String())
	}
	var feed integrations.ErrorsResponse
	decodeBody(t, response, &feed)
	if feed.SchemaVersion != integrations.SchemaVersion || feed.NextSeq != 2 || len(feed.Errors) != 2 ||
		feed.Errors[0].Seq != 1 || feed.Errors[0].Kind != "dependency_error" || feed.Errors[0].SessionID != "session-a" ||
		feed.Errors[1].Seq != 2 || feed.Errors[1].Kind != "daemon_error" || feed.Errors[1].Machine == "" {
		t.Fatalf("feed = %#v", feed)
	}

	paged := serve(t, daemon.handler, http.MethodGet, "/api/errors?since=1", nil, "127.0.0.1:4321", nil)
	decodeBody(t, paged, &feed)
	if paged.Code != http.StatusOK || feed.NextSeq != 2 || len(feed.Errors) != 1 || feed.Errors[0].Seq != 2 {
		t.Fatalf("paged status=%d feed=%#v", paged.Code, feed)
	}
	invalid := serve(t, daemon.handler, http.MethodGet, "/api/errors?since=-1", nil, "127.0.0.1:4321", nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	path := filepath.Join(daemon.config.StateRoot, "errors.jsonl")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(encoded)), "\n")
	if len(lines) != 2 {
		t.Fatalf("error log lines=%d contents=%q", len(lines), encoded)
	}
	assertMode(t, path, 0o600)
	for index, line := range lines {
		var event integrations.ErrorEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Seq != uint64(index+1) {
			t.Fatalf("line %d event=%#v err=%v", index+1, event, err)
		}
	}
}

func TestErrorsRouteObservesNonzeroRunnerExitOnce(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("created session missing")
	}
	attachment := session.Attach(state.AttachOptions{})
	defer attachment.Cancel()
	code := 17
	daemon.launcher.Runner(info.ID).Emit(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &code}})
	if event := <-attachment.Events; event.Kind != proto.EventExit {
		t.Fatalf("terminal event = %#v", event)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response := serve(t, daemon.handler, http.MethodGet, "/api/errors", nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("feed status=%d body=%s", response.Code, response.Body.String())
		}
		var feed integrations.ErrorsResponse
		decodeBody(t, response, &feed)
		if len(feed.Errors) != 1 || feed.NextSeq != 1 || feed.Errors[0].Kind != "runner_exit" ||
			feed.Errors[0].SessionID != info.ID || feed.Errors[0].Summary != "runner exited with code 17" {
			t.Fatalf("attempt %d feed=%#v", attempt, feed)
		}
	}
}

func TestErrorsRouteTracksRunnerLostAfterInitialPoll(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("created session missing")
	}
	attachment := session.Attach(state.AttachOptions{})
	defer attachment.Cancel()
	initial := serve(t, daemon.handler, http.MethodGet, "/api/errors", nil, "127.0.0.1:4321", nil)
	var feed integrations.ErrorsResponse
	decodeBody(t, initial, &feed)
	if initial.Code != http.StatusOK || len(feed.Errors) != 0 {
		t.Fatalf("initial status=%d feed=%#v", initial.Code, feed)
	}

	daemon.launcher.Runner(info.ID).Emit(proto.Event{Kind: proto.EventRunnerLost})
	if event := <-attachment.Events; event.Kind != proto.EventRunnerLost || event.Exit.Reason != "runner-lost" {
		t.Fatalf("terminal event = %#v", event)
	}
	deadline := time.Now().Add(time.Second)
	for {
		response := serve(t, daemon.handler, http.MethodGet, "/api/errors", nil, "127.0.0.1:4321", nil)
		decodeBody(t, response, &feed)
		if len(feed.Errors) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner_lost event was not recorded: %#v", feed)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if feed.NextSeq != 1 || feed.Errors[0].Kind != "runner_lost" ||
		feed.Errors[0].SessionID != info.ID || feed.Errors[0].Summary != "runner connection lost" {
		t.Fatalf("feed = %#v", feed)
	}
}
