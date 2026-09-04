package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type continuationCatalogRegistry struct {
	sessionService
	models []codexapp.Model
}

func (r continuationCatalogRegistry) CodexModelOptions(context.Context) ([]codexapp.Model, error) {
	return r.models, nil
}

func continuationCatalog(registry sessionService) continuationCatalogRegistry {
	return continuationCatalogRegistry{
		sessionService: registry,
		models: []codexapp.Model{{
			ID: "gpt-next", Model: "gpt-next", DisplayName: "GPT Next", IsDefault: true,
			DefaultReasoningEffort: "medium",
			SupportedReasoningEfforts: []codexapp.ReasoningEffortOption{
				{ReasoningEffort: "low"}, {ReasoningEffort: "medium"}, {ReasoningEffort: "high"},
			},
		}},
	}
}

func writeContinuationHistory(t *testing.T, daemon testDaemon, home, id string) {
	t.Helper()
	conversation := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-08-05T09:00:00Z","message":{"role":"user","content":"abcdefgh"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-08-05T09:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"ijkl"}]}}`,
	}, "\n") + "\n"
	writeClaudeHistoryFixture(t, daemon, home, id, "Frozen review", []byte(conversation))
}

func TestContinuationPreviewCountsExportedHistoryWithoutCreatingSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONS_CONTINUATION_TOKEN_THRESHOLD", "2")
	daemon := newTestDaemon(t)
	id := "11111111-2222-4333-8444-555555555555"
	writeContinuationHistory(t, daemon, home, id)

	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/continuation/preview",
		strings.NewReader(`{"historyId":"`+id+`","destinationProvider":"codex"}`), "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", response.Code, response.Body.String())
	}
	var preview continuationPreview
	decodeBody(t, response, &preview)
	if preview.Conversation != "Frozen review" || preview.MessageCount != 2 ||
		preview.TotalMessageCount != 2 || preview.CharacterCount != 12 ||
		preview.EstimatedTokens != 3 || preview.ThresholdTokens != 2 || !preview.SourceUntouched {
		t.Fatalf("preview = %+v", preview)
	}
	if len(daemon.launcher.Launches) != 0 {
		t.Fatalf("dry run created %d sessions", len(daemon.launcher.Launches))
	}

	limited := serve(t, daemon.handler, http.MethodPost, "/api/recovery/continuation/preview",
		strings.NewReader(`{"historyId":"`+id+`","destinationProvider":"codex","messageLimit":1}`), "127.0.0.1:1", nil)
	decodeBody(t, limited, &preview)
	if preview.MessageCount != 1 || preview.CharacterCount != 4 || preview.EstimatedTokens != 1 || !preview.Limited {
		t.Fatalf("limited preview = %+v", preview)
	}
}

func TestContinuationJobPublishesStagesFaultAndFirstReply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	id := "22222222-3333-4444-8555-666666666666"
	writeContinuationHistory(t, daemon, home, id)
	daemon.handler.registry = continuationCatalog(daemon.registry)

	job := startContinuationJob(t, daemon, id)
	job = waitContinuationJob(t, daemon, job.ID, func(view continuationJobView) bool {
		return view.Stage == "first-reply" && view.LaneID != ""
	})
	created, ok := daemon.registry.Get(job.LaneID)
	if !ok {
		t.Fatalf("created continuation %q is not live", job.LaneID)
	}
	created.SetProviderFault("codex", providerfault.Fault{Kind: "rate-limited", Detail: "Codex is busy"}, time.Now().UnixMilli())
	job = waitContinuationJob(t, daemon, job.ID, func(view continuationJobView) bool {
		return view.FailureKind == "rate-limited"
	})
	if job.FailureDetail != "Codex is busy" {
		t.Fatalf("provider fault = %+v", job)
	}
	created.ClearProviderFault()
	created.SetIdleResult(state.IdleReasonCompleted, "", "Ready to continue", time.Now().Add(time.Second).UnixMilli())
	job = waitContinuationJob(t, daemon, job.ID, func(view continuationJobView) bool {
		return view.Status == "succeeded"
	})
	stages := make([]string, 0, len(job.Events))
	for _, event := range job.Events {
		stages = append(stages, event.Stage)
	}
	want := []string{"exporting-history", "creating-session", "provider-starting", "first-reply"}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("job stages = %v, want %v", stages, want)
	}
	info := created.Info()
	if info.Model != "gpt-next" || info.Effort != "medium" || info.ImportedMessageCount != 2 {
		t.Fatalf("created model/history = %+v", info)
	}
}

func TestContinuationJobCancelEndsOnlyNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	id := "33333333-4444-4555-8666-777777777777"
	writeContinuationHistory(t, daemon, home, id)
	daemon.handler.registry = continuationCatalog(daemon.registry)

	job := startContinuationJob(t, daemon, id)
	job = waitContinuationJob(t, daemon, job.ID, func(view continuationJobView) bool {
		return view.Stage == "first-reply" && view.LaneID != ""
	})
	response := serve(t, daemon.handler, http.MethodDelete,
		"/api/recovery/continuation/jobs/"+job.ID, nil, "127.0.0.1:1", nil)
	decodeBody(t, response, &job)
	if job.Status != "canceled" || !strings.Contains(job.StageText, "original conversation was not changed") {
		t.Fatalf("canceled job = %+v", job)
	}
	created, ok := daemon.registry.Get(job.LaneID)
	if !ok {
		t.Fatalf("new session %q disappeared instead of recording its end", job.LaneID)
	}
	if !created.Info().Exited {
		t.Fatalf("new session was not ended: %+v", created.Info())
	}
}

func startContinuationJob(t *testing.T, daemon testDaemon, historyID string) continuationJobView {
	t.Helper()
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/continuation/jobs",
		strings.NewReader(`{"historyId":"`+historyID+`","destinationProvider":"codex","model":"gpt-next","effort":"medium"}`),
		"127.0.0.1:1", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start status=%d body=%s", response.Code, response.Body.String())
	}
	var job continuationJobView
	decodeBody(t, response, &job)
	return job
}

func waitContinuationJob(
	t *testing.T, daemon testDaemon, id string, ready func(continuationJobView) bool,
) continuationJobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := serve(t, daemon.handler, http.MethodGet,
			"/api/recovery/continuation/jobs/"+id, nil, "127.0.0.1:1", nil)
		var job continuationJobView
		if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if ready(job) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("continuation job %s did not reach expected state", id)
	return continuationJobView{}
}
