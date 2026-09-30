package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

func TestDailyRejectsInvalidDateAndMethod(t *testing.T) {
	daemon := newTestDaemon(t)
	badDate := serve(t, daemon.handler, http.MethodGet, "/api/daily?date=yesterday", nil, "127.0.0.1:1", nil)
	if badDate.Code != http.StatusBadRequest {
		t.Fatalf("bad date: status=%d body=%s", badDate.Code, badDate.Body.String())
	}
	post := serve(t, daemon.handler, http.MethodPost, "/api/daily", nil, "127.0.0.1:1", nil)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: status=%d body=%s", post.Code, post.Body.String())
	}
}

func TestDailyIncludesProviderWorkOutsideSessions(t *testing.T) {
	daemon := newTestDaemon(t)
	codexRoot := filepath.Join(daemon.root, ".codex", "sessions", "2026", "07", "22")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	providerID := "11111111-1111-4111-8111-111111111111"
	stamp := func(hour, minute int) string {
		return time.Date(2026, time.July, 22, hour, minute, 0, 0, time.Local).Format(time.RFC3339Nano)
	}
	log := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":"/work/sessions","originator":"Codex Desktop"}}
{"timestamp":%q,"type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5.2-codex"}}
{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Make Daily cover this work"}]}}
{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Daily now includes local Codex work outside Sessions"}]}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":400,"cached_input_tokens":100,"output_tokens":50}}}}
`, stamp(10, 0), providerID, stamp(10, 0), stamp(10, 1), stamp(10, 4), stamp(10, 5))
	if err := os.WriteFile(filepath.Join(codexRoot, "rollout.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.handler.usage = usage.NewService(usage.Options{
		Path:           filepath.Join(daemon.root, "usage.sqlite3"),
		CodexRoots:     []string{filepath.Join(daemon.root, ".codex", "sessions")},
		RunnerStateDir: daemon.config.RunnerStateDir,
	})
	defer daemon.handler.usage.Close()

	response := serve(t, daemon.handler, http.MethodGet, "/api/daily?date=2026-07-22", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("daily: status=%d body=%s", response.Code, response.Body.String())
	}
	var day dailyDayResponse
	decodeBody(t, response, &day)
	if len(day.Activities) != 1 {
		t.Fatalf("activities = %#v", day.Activities)
	}
	activity := day.Activities[0]
	if activity.Source != "provider" || activity.ProviderSessionID != providerID || activity.Origin != "Codex Desktop" ||
		activity.Description != "Make Daily cover this work" || activity.Summary != "Daily now includes local Codex work outside Sessions" {
		t.Fatalf("external activity = %#v", activity)
	}
	if day.Usage.Entries != 1 || day.Usage.Tokens.Input != 300 || day.Usage.Tokens.CacheRead != 100 {
		t.Fatalf("usage = %#v", day.Usage)
	}
}
