package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
	"github.com/somewhere-tech/sessions/runtime/internal/smartsearch"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestSearchRouteUsesNormalizedKnownSessionHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	daemon.config.UserStateRoot = filepath.Join(daemon.root, "user-state")
	daemon.handler.config.UserStateRoot = daemon.config.UserStateRoot
	if err := os.MkdirAll(daemon.config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 17, 18, 0, 0, 0, time.UTC)
	claudeID := "aaaaaaaa-1111-4222-8333-444444444444"
	claudeCWD := filepath.Join(daemon.root, "claude-worktree")
	writeSearchMetadata(t, daemon.config.RunnerStateDir, claudeID, "claude fixture", "claude", []string{"--session-id", claudeID}, claudeCWD, now)
	claudePath := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(claudeCWD), claudeID+".jsonl")
	writeSearchJSONL(t, claudePath, []map[string]any{
		{"type": "user", "timestamp": "2026-07-17T18:00:01Z", "message": map[string]any{"role": "user", "content": "Find the Aurora emails phrase"}},
		{"type": "assistant", "timestamp": "2026-07-17T18:00:02Z", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Claude saw AURORA."}}}},
	})
	if err := os.Chtimes(claudePath, now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	codexID := "bbbbbbbb-1111-4222-8333-444444444444"
	resumeID := "01234567-89ab-4cde-8fab-0123456789ab"
	codexCWD := filepath.Join(daemon.root, "codex-worktree")
	writeSearchMetadata(t, daemon.config.RunnerStateDir, codexID, "codex fixture", "codex", []string{"resume", resumeID}, codexCWD, now)
	rolloutPath := filepath.Join(home, ".codex", "sessions", "2026", "07", "17", "rollout-"+resumeID+".jsonl")
	writeSearchJSONL(t, rolloutPath, []map[string]any{
		{"type": "session_meta", "timestamp": now.Format(time.RFC3339Nano), "payload": map[string]any{"cwd": codexCWD, "timestamp": now.Format(time.RFC3339Nano)}},
		{"type": "response_item", "timestamp": "2026-07-17T18:01:01Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Codex asks about aurora"}}}},
		{"type": "response_item", "timestamp": "2026-07-17T18:01:02Z", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Codex answer number 73"}}}},
	})
	if err := os.Chtimes(rolloutPath, now.Add(2*time.Minute), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/api/search?q=Aurora", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", response.Code, response.Body.String())
	}
	var result historysearch.Response
	decodeBody(t, response, &result)
	claudeMatches, codexMatches, highlighted := 0, 0, false
	for _, match := range result.Matches {
		switch match.SessionID {
		case claudeID:
			claudeMatches++
			highlighted = highlighted || strings.Contains(match.Snippet, "[[AURORA]]")
		case codexID:
			codexMatches++
		}
	}
	if result.Total != 3 || len(result.Matches) != 3 || claudeMatches != 2 || codexMatches != 1 || !highlighted {
		t.Fatalf("result = %#v", result)
	}
	ranked := serve(t, daemon.handler, http.MethodGet, "/api/search?q=email&ranked=1", nil, "127.0.0.1:4321", nil)
	decodeBody(t, ranked, &result)
	if ranked.Code != http.StatusOK || result.Total != 1 ||
		!strings.Contains(result.Matches[0].Snippet, "[[emails]]") {
		t.Fatalf("ranked status=%d result=%#v", ranked.Code, result)
	}
	if _, err := os.Stat(filepath.Join(daemon.config.UserStateRoot, "search-index.db")); err != nil {
		t.Fatalf("search index: %v", err)
	}

	parameters := url.Values{
		"q": {`number [0-9]+`}, "regex": {"true"}, "session": {codexID[:8]},
		"role": {"assistant"}, "tool": {"codex"}, "limit": {"1"},
	}
	filtered := serve(t, daemon.handler, http.MethodGet, "/api/search?"+parameters.Encode(), nil, "127.0.0.1:4321", nil)
	decodeBody(t, filtered, &result)
	if filtered.Code != http.StatusOK || result.Total != 1 ||
		!strings.Contains(result.Matches[0].Snippet, "[[number 73]]") ||
		result.Matches[0].Timestamp == nil || *result.Matches[0].Timestamp != "2026-07-17T18:01:02Z" {
		t.Fatalf("filtered status=%d result=%#v", filtered.Code, result)
	}

	for _, target := range []string{
		"/api/search", "/api/search?q=(&regex=true", "/api/search?q=x&role=system", "/api/search?q=x&limit=0",
		"/api/search?q=x&ranked=maybe", "/api/search?q=x&ranked=true&regex=true",
	} {
		invalid := serve(t, daemon.handler, http.MethodGet, target, nil, "127.0.0.1:4321", nil)
		if invalid.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d body=%s", target, invalid.Code, invalid.Body.String())
		}
	}
	method := serve(t, daemon.handler, http.MethodPost, "/api/search?q=x", nil, "127.0.0.1:4321", nil)
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d body=%s", method.Code, method.Body.String())
	}
}

func TestSmartSearchSettingsAndPlan(t *testing.T) {
	daemon := newTestDaemon(t)
	var provider, prompt string
	daemon.handler.smartSearch = smartsearch.NewServiceWithRunner(func(_ context.Context, gotProvider, gotPrompt string) (string, error) {
		provider, prompt = gotProvider, gotPrompt
		return `{"query":"apple AND signing"}`, nil
	})

	defaults := serve(t, daemon.handler, http.MethodGet, "/api/ai/settings", nil, "127.0.0.1:4321", nil)
	var settings state.AISettings
	decodeBody(t, defaults, &settings)
	if defaults.Code != http.StatusOK || settings.Provider != state.AIProviderCodex {
		t.Fatalf("defaults status=%d settings=%#v", defaults.Code, settings)
	}

	updated := serve(t, daemon.handler, http.MethodPut, "/api/ai/settings", strings.NewReader(`{"provider":"claude"}`), "127.0.0.1:4321", nil)
	decodeBody(t, updated, &settings)
	if updated.Code != http.StatusOK || settings.Provider != state.AIProviderClaude {
		t.Fatalf("updated status=%d settings=%#v", updated.Code, settings)
	}

	planned := serve(t, daemon.handler, http.MethodPost, "/api/search/plan", strings.NewReader(`{"query":"the session where I discussed Apple signing"}`), "127.0.0.1:4321", nil)
	var plan struct {
		Provider string `json:"provider"`
		Query    string `json:"query"`
	}
	decodeBody(t, planned, &plan)
	if planned.Code != http.StatusOK || provider != state.AIProviderClaude || plan.Provider != state.AIProviderClaude || plan.Query != "apple AND signing" || !strings.Contains(prompt, "Apple signing") {
		t.Fatalf("status=%d provider=%q plan=%#v prompt=%q", planned.Code, provider, plan, prompt)
	}

	bad := serve(t, daemon.handler, http.MethodPost, "/api/search/plan", strings.NewReader(`{"query":""}`), "127.0.0.1:4321", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("empty status=%d body=%s", bad.Code, bad.Body.String())
	}
	blank := serve(t, daemon.handler, http.MethodPost, "/api/search/plan", strings.NewReader(`{"query":"   "}`), "127.0.0.1:4321", nil)
	if blank.Code != http.StatusBadRequest {
		t.Fatalf("blank status=%d body=%s", blank.Code, blank.Body.String())
	}
	daemon.handler.smartSearch = smartsearch.NewServiceWithRunner(func(context.Context, string, string) (string, error) {
		return "", smartsearch.ErrBusy
	})
	busy := serve(t, daemon.handler, http.MethodPost, "/api/search/plan", strings.NewReader(`{"query":"another search"}`), "127.0.0.1:4321", nil)
	if busy.Code != http.StatusTooManyRequests || busy.Header().Get("Retry-After") != "2" {
		t.Fatalf("busy status=%d retry=%q body=%s", busy.Code, busy.Header().Get("Retry-After"), busy.Body.String())
	}
}

func writeSearchMetadata(t *testing.T, runnerDir, id, name, command string, args []string, cwd string, created time.Time) {
	t.Helper()
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(filepath.Join(runnerDir, id+".json"), state.Metadata{
		ID: id, Name: name, Cmd: command, Args: args, Cwd: cwd,
		CreatedAt: created.UnixMilli(), SockPath: filepath.Join(runnerDir, id+".sock"),
	}); err != nil {
		t.Fatal(err)
	}
}

func writeSearchJSONL(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
