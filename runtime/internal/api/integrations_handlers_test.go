package api

import (
	"bytes"
	"context"
	"encoding/json"
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
		`{"type":"user","uuid":"u1","timestamp":"2026-07-16T17:01:00Z","message":{"role":"user","content":"Recall this fixture"}}`,
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
	if listed.ID != id || listed.Name != "fixture recall" || listed.Tool != "claude" || listed.CWD != cwd ||
		listed.Machine == "" || listed.CreatedAt != created.UnixMilli() || listed.LastActivityAt != modified.UnixMilli() ||
		listed.MessageCount != 2 || !listed.ConversationAvailable {
		t.Fatalf("listed session = %#v", listed)
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
