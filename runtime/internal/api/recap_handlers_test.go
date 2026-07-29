package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/recap"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

func TestRecapSettingsAndGeneration(t *testing.T) {
	daemon := newTestDaemon(t)
	if err := state.SaveSettings(daemon.handler.lan.settingsPath, state.Settings{LAN: true}); err != nil {
		t.Fatal(err)
	}
	daemon.handler.recaps = recap.NewServiceWithRunner(daemon.config.StateRoot, func(_ context.Context, settings state.RecapSettings, prompt string) (string, error) {
		if settings.Provider != state.RecapProviderCodex {
			t.Fatalf("settings = %#v", settings)
		}
		if !strings.Contains(prompt, `"date":"2026-07-22"`) {
			t.Fatalf("prompt = %q", prompt)
		}
		return "# Daily recap\n\nA quiet but valid day.", nil
	})

	off := serve(t, daemon.handler, http.MethodGet, "/api/recap/settings", nil, "127.0.0.1:1", nil)
	if off.Code != http.StatusOK || !strings.Contains(off.Body.String(), `"provider":"off"`) {
		t.Fatalf("off settings: status=%d body=%s", off.Code, off.Body.String())
	}
	updated := serve(t, daemon.handler, http.MethodPut, "/api/recap/settings", strings.NewReader(`{"provider":"codex"}`), "127.0.0.1:1", nil)
	if updated.Code != http.StatusOK {
		t.Fatalf("update settings: status=%d body=%s", updated.Code, updated.Body.String())
	}
	settings, err := state.LoadSettings(daemon.handler.lan.settingsPath)
	if err != nil || !settings.LAN || settings.EffectiveRecap().Provider != state.RecapProviderCodex {
		t.Fatalf("preserved settings = %#v, %v", settings, err)
	}

	generated := serve(t, daemon.handler, http.MethodPost, "/api/recap/generate", strings.NewReader(`{"date":"2026-07-22"}`), "127.0.0.1:1", nil)
	if generated.Code != http.StatusOK || !strings.Contains(generated.Body.String(), "A quiet but valid day") {
		t.Fatalf("generate: status=%d body=%s", generated.Code, generated.Body.String())
	}
	loaded := serve(t, daemon.handler, http.MethodGet, "/api/recap?date=2026-07-22", nil, "127.0.0.1:1", nil)
	if loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), `"provider":"codex"`) {
		t.Fatalf("loaded: status=%d body=%s", loaded.Code, loaded.Body.String())
	}
	dates := serve(t, daemon.handler, http.MethodGet, "/api/recap/dates", nil, "127.0.0.1:1", nil)
	if dates.Code != http.StatusOK || !strings.Contains(dates.Body.String(), `"dates":["2026-07-22"]`) {
		t.Fatalf("dates: status=%d body=%s", dates.Code, dates.Body.String())
	}
	badDate := serve(t, daemon.handler, http.MethodGet, "/api/recap?date=yesterday", nil, "127.0.0.1:1", nil)
	if badDate.Code != http.StatusBadRequest {
		t.Fatalf("bad date: status=%d body=%s", badDate.Code, badDate.Body.String())
	}
}

func TestRecapKeepsSavedDocumentWhenFactsChange(t *testing.T) {
	daemon := newTestDaemon(t)
	if err := state.SaveSettings(daemon.handler.lan.settingsPath, state.Settings{Recap: &state.RecapSettings{Provider: state.RecapProviderCodex}}); err != nil {
		t.Fatal(err)
	}
	daemon.handler.recaps = recap.NewServiceWithRunner(daemon.config.StateRoot, func(_ context.Context, _ state.RecapSettings, _ string) (string, error) {
		return "# Saved daily recap", nil
	})
	generated := serve(t, daemon.handler, http.MethodPost, "/api/recap/generate", strings.NewReader(`{"date":"2026-07-22"}`), "127.0.0.1:1", nil)
	if generated.Code != http.StatusOK {
		t.Fatalf("generate: status=%d body=%s", generated.Code, generated.Body.String())
	}
	if err := state.UpdateSettings(daemon.handler.lan.settingsPath, func(settings *state.Settings) error {
		settings.Recap = &state.RecapSettings{Provider: state.RecapProviderClaude}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded := serve(t, daemon.handler, http.MethodGet, "/api/recap?date=2026-07-22", nil, "127.0.0.1:1", nil)
	if loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), `"documentStale":true`) ||
		!strings.Contains(loaded.Body.String(), "Saved daily recap") {
		t.Fatalf("stale saved recap: status=%d body=%s", loaded.Code, loaded.Body.String())
	}
}

func TestRecapIncludesProviderWorkOutsideSessions(t *testing.T) {
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
{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Make Today cover this work"}]}}
{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Today now includes local Codex work outside Sessions"}]}}
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

	response := serve(t, daemon.handler, http.MethodGet, "/api/recap?date=2026-07-22", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("recap: status=%d body=%s", response.Code, response.Body.String())
	}
	var day recapDayResponse
	decodeBody(t, response, &day)
	if len(day.Activities) != 1 {
		t.Fatalf("activities = %#v", day.Activities)
	}
	activity := day.Activities[0]
	if activity.Source != "provider" || activity.ProviderSessionID != providerID || activity.Origin != "Codex Desktop" ||
		activity.Description != "Make Today cover this work" || activity.Summary != "Today now includes local Codex work outside Sessions" {
		t.Fatalf("external activity = %#v", activity)
	}
	if day.Usage.Entries != 1 || day.Usage.Tokens.Input != 300 || day.Usage.Tokens.CacheRead != 100 {
		t.Fatalf("usage = %#v", day.Usage)
	}
}
