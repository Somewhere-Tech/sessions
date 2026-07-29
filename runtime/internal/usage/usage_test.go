package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestReportIndexesClaudeAndCodexIncrementallyWithTags(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, ".claude", "projects")
	codexRoot := filepath.Join(root, ".codex", "sessions")
	runnerRoot := filepath.Join(root, "runners")
	for _, path := range []string{filepath.Join(claudeRoot, "project"), filepath.Join(codexRoot, "2026", "07", "20"), runnerRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	claudePath := filepath.Join(claudeRoot, "project", "claude-session.jsonl")
	claude := `{"timestamp":"2026-07-20T08:00:00Z","sessionId":"claude-session","requestId":"request-1","costUSD":0.12,"message":{"id":"message-1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":10,"cache_read_input_tokens":30}}}
{"timestamp":"2026-07-20T08:00:01Z","sessionId":"claude-session","requestId":"request-1","costUSD":0.15,"message":{"id":"message-1","model":"claude-sonnet-4-6","usage":{"input_tokens":120,"output_tokens":30,"cache_creation_input_tokens":10,"cache_read_input_tokens":40}}}
`
	if err := os.WriteFile(claudePath, []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(codexRoot, "2026", "07", "20", "rollout.jsonl")
	codex := `{"timestamp":"2026-07-20T09:00:00Z","type":"session_meta","payload":{"id":"codex-session"}}
{"timestamp":"2026-07-20T09:00:01Z","type":"turn_context","payload":{"model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-20T09:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":250,"output_tokens":125,"reasoning_output_tokens":25}}}}
`
	if err := os.WriteFile(codexPath, []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(filepath.Join(runnerRoot, "sessions-id.json"), state.Metadata{
		ID: "sessions-id", Cmd: "claude", Args: []string{"--session-id", "claude-session"}, Cwd: root,
		CreatedAt: 1, SockPath: filepath.Join(runnerRoot, "sessions-id.sock"), ClaudeSessionID: "claude-session",
		Tags: map[string]string{"product": "Sessions", "team": "native"},
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(Options{Path: filepath.Join(root, "usage.sqlite3"), ClaudeRoots: []string{claudeRoot},
		CodexRoots: []string{codexRoot}, RunnerStateDir: runnerRoot,
		Machine: "test-mac",
		Now:     func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	defer service.Close()
	report, err := service.Report(context.Background(), ReportOptions{Group: "daily", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 || report.Totals.Entries != 2 {
		t.Fatalf("daily report = %#v", report)
	}
	if report.SchemaVersion != 1 || report.Machine != "test-mac" {
		t.Fatalf("report identity = schema %d machine %q", report.SchemaVersion, report.Machine)
	}
	if report.Totals.Tokens.Input != 870 || report.Totals.Tokens.CacheRead != 290 || report.Totals.Tokens.Output != 155 {
		t.Fatalf("daily tokens = %#v", report.Totals.Tokens)
	}
	if report.Totals.Tokens.Reasoning != 25 || report.Totals.Tokens.Total() != 1_325 {
		t.Fatalf("reasoning token subset = %#v", report.Totals.Tokens)
	}
	if report.Totals.RecordedCostUSD != .15 || report.Totals.CostUSD <= .15 {
		t.Fatalf("daily costs = recorded %.6f selected %.6f", report.Totals.RecordedCostUSD, report.Totals.CostUSD)
	}
	db, err := service.database(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE usage_sources SET parser_state = '{}'; UPDATE usage_entries SET calculated_cost_usd = 999`); err != nil {
		t.Fatal(err)
	}
	repriced, err := service.Report(context.Background(), ReportOptions{Group: "daily", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if repriced.Scan.FilesRead != 2 || repriced.Totals.CalculatedCostUSD >= 1 {
		t.Fatalf("pricing schema reindex = scan %#v totals %#v", repriced.Scan, repriced.Totals)
	}
	tagReport, err := service.Report(context.Background(), ReportOptions{Group: "tag", Dimension: "product", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if len(tagReport.Rows) != 2 {
		t.Fatalf("tag report = %#v", tagReport.Rows)
	}
	tagKeys := map[string]bool{}
	for _, row := range tagReport.Rows {
		tagKeys[row.Key] = true
	}
	if !tagKeys["Sessions"] || !tagKeys["(untagged)"] {
		t.Fatalf("tag report = %#v", tagReport.Rows)
	}
	sessionReport, err := service.Report(context.Background(), ReportOptions{Group: "session", Mode: ModeDisplay})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionReport.Rows) != 2 || sessionReport.Rows[0].SessionID != "sessions-id" || sessionReport.Rows[0].Tags["team"] != "native" {
		t.Fatalf("session report = %#v", sessionReport.Rows)
	}
	providerReport, err := service.Report(context.Background(), ReportOptions{Group: "provider", Mode: ModeCalculate})
	if err != nil || len(providerReport.Rows) != 2 || providerReport.Rows[0].Provider == "" {
		t.Fatalf("provider report err=%v rows=%#v", err, providerReport.Rows)
	}
	modelReport, err := service.Report(context.Background(), ReportOptions{Group: "model", Mode: ModeCalculate})
	if err != nil || len(modelReport.Rows) != 2 || len(modelReport.Rows[0].Models) != 1 {
		t.Fatalf("model report err=%v rows=%#v", err, modelReport.Rows)
	}

	file, err := os.OpenFile(codexPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"timestamp":"2026-07-20T09:05:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":200,"cached_input_tokens":50,"output_tokens":25,"reasoning_output_tokens":10}}}}` + "\n")
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	incremental, err := service.Report(context.Background(), ReportOptions{Group: "daily", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Scan.FilesRead != 1 || incremental.Scan.LinesRead != 1 || incremental.Totals.Entries != 3 || incremental.Totals.Tokens.Reasoning != 35 {
		t.Fatalf("incremental report scan=%#v total=%#v", incremental.Scan, incremental.Totals)
	}
}

func TestUsageGroupingUsesLocalCalendarBoundaries(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("Pacific test", -7*60*60)
	defer func() { time.Local = previous }()

	stamp := time.Date(2026, 7, 20, 6, 30, 0, 0, time.UTC) // Sunday 23:30 locally.
	daily, _ := usageGroupKey(ReportOptions{Group: "daily"}, stamp, "codex", "session", "gpt", sessionBinding{})
	weekly, _ := usageGroupKey(ReportOptions{Group: "weekly"}, stamp, "codex", "session", "gpt", sessionBinding{})
	monthly, _ := usageGroupKey(ReportOptions{Group: "monthly"}, stamp, "codex", "session", "gpt", sessionBinding{})
	if daily != "2026-07-19" || weekly != "2026-07-13" || monthly != "2026-07" {
		t.Fatalf("local groups = daily %q weekly %q monthly %q", daily, weekly, monthly)
	}
}

func TestUsageDatabaseMigratesReasoningColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_entries (
event_key TEXT PRIMARY KEY, source_path TEXT NOT NULL, source_offset INTEGER NOT NULL,
provider TEXT NOT NULL, provider_session_id TEXT NOT NULL, timestamp_ms INTEGER NOT NULL,
model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
cache_creation_tokens INTEGER NOT NULL, cache_read_tokens INTEGER NOT NULL,
recorded_cost_usd REAL, calculated_cost_usd REAL NOT NULL, pricing_found INTEGER NOT NULL
);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	service := NewService(Options{Path: path})
	defer service.Close()
	db, err = service.database(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('usage_entries') WHERE name = 'reasoning_tokens'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reasoning_tokens columns = %d", count)
	}
}

func TestStructuredUsageIsVisibleBeforeBackfillAndDeduplicatesProviderLogs(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, ".claude", "projects", "project")
	codexRoot := filepath.Join(root, ".codex", "sessions", "2026", "07", "20")
	for _, path := range []string{claudeRoot, codexRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(Options{
		Path: filepath.Join(root, "usage.sqlite3"), ClaudeRoots: []string{filepath.Dir(claudeRoot)},
		CodexRoots: []string{filepath.Join(root, ".codex", "sessions")}, Machine: "test-mac",
	})
	defer service.Close()

	claudeLive := json.RawMessage(`{"timestamp":"2026-07-20T08:00:00Z","session_id":"claude-live","costUSD":0.12,"message":{"id":"message-live","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30}}}`)
	if err := service.RecordStructured(context.Background(), state.SessionInfo{ID: "sessions-claude", Tool: state.ToolClaude, ClaudeSessionID: "claude-live"}, claudeLive); err != nil {
		t.Fatal(err)
	}
	codexLive := json.RawMessage(`{"type":"codex","subtype":"token_count","source":"codex-app-server","timestamp":"2026-07-20T09:00:02Z","conversationId":"codex-live","turnId":"turn-live","usage":{"last":{"inputTokens":1000,"cachedInputTokens":250,"outputTokens":125,"reasoningOutputTokens":25,"totalTokens":1125},"total":{"inputTokens":1000,"cachedInputTokens":250,"outputTokens":125,"reasoningOutputTokens":25,"totalTokens":1125}}}`)
	if err := service.RecordStructured(context.Background(), state.SessionInfo{ID: "sessions-codex", Tool: state.ToolCodex, ConversationID: "codex-live", Model: "gpt-5.2-codex"}, codexLive); err != nil {
		t.Fatal(err)
	}
	db, err := service.database(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var immediate int
	if err := db.QueryRow(`SELECT count(*) FROM usage_entries WHERE source_path LIKE 'live://%'`).Scan(&immediate); err != nil {
		t.Fatal(err)
	}
	if immediate != 2 {
		t.Fatalf("live usage entries = %d, want 2", immediate)
	}

	claudeLog := `{"timestamp":"2026-07-20T08:00:00Z","sessionId":"claude-live","requestId":"request-live","costUSD":0.12,"message":{"id":"message-live","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30}}}` + "\n"
	if err := os.WriteFile(filepath.Join(claudeRoot, "claude-live.jsonl"), []byte(claudeLog), 0o600); err != nil {
		t.Fatal(err)
	}
	codexLog := `{"timestamp":"2026-07-20T09:00:00Z","type":"session_meta","payload":{"id":"codex-live"}}
{"timestamp":"2026-07-20T09:00:01Z","type":"turn_context","payload":{"turn_id":"turn-live","model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-20T09:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":250,"output_tokens":125,"reasoning_output_tokens":25}}}}
`
	if err := os.WriteFile(filepath.Join(codexRoot, "rollout-live.jsonl"), []byte(codexLog), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := service.Report(context.Background(), ReportOptions{Group: "daily", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Entries != 2 || report.Totals.Tokens.Input != 850 || report.Totals.Tokens.Output != 145 || report.Totals.Tokens.Reasoning != 25 {
		t.Fatalf("live plus backfill report = %#v", report.Totals)
	}
	var liveSources int
	if err := db.QueryRow(`SELECT count(*) FROM usage_entries WHERE source_path LIKE 'live://%'`).Scan(&liveSources); err != nil {
		t.Fatal(err)
	}
	if liveSources != 0 {
		t.Fatalf("provider backfill did not enrich live rows: %d live sources remain", liveSources)
	}
}

func TestCodexForkReplayDoesNotRedateParentUsage(t *testing.T) {
	root := t.TempDir()
	codexRoot := filepath.Join(root, ".codex", "sessions")
	originalDir := filepath.Join(codexRoot, "2026", "07", "21")
	forkDir := filepath.Join(codexRoot, "2026", "07", "22")
	for _, dir := range []string{originalDir, forkDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parentID := "11111111-1111-4111-8111-111111111111"
	childID := "22222222-2222-4222-8222-222222222222"
	original := `{"timestamp":"2026-07-21T18:00:00Z","type":"session_meta","payload":{"id":"` + parentID + `"}}
{"timestamp":"2026-07-21T18:00:01Z","type":"turn_context","payload":{"turn_id":"parent-turn","model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-21T18:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":250,"output_tokens":125,"reasoning_output_tokens":25}}}}
`
	if err := os.WriteFile(filepath.Join(originalDir, "rollout-parent.jsonl"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	fork := `{"timestamp":"2026-07-22T18:00:00Z","type":"session_meta","payload":{"id":"` + childID + `","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `"}}}}}
{"timestamp":"2026-07-22T18:00:00Z","type":"session_meta","payload":{"id":"` + parentID + `"}}
{"timestamp":"2026-07-22T18:00:00Z","type":"turn_context","payload":{"turn_id":"parent-turn","model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-22T18:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":250,"output_tokens":125,"reasoning_output_tokens":25}}}}
{"timestamp":"2026-07-22T18:00:30Z","type":"event_msg","payload":{"type":"task_started","turn_id":"child-turn"}}
{"timestamp":"2026-07-22T18:01:00Z","type":"turn_context","payload":{"turn_id":"child-turn","model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-22T18:01:01Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":400,"cached_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":10}}}}
`
	if err := os.WriteFile(filepath.Join(forkDir, "rollout-child.jsonl"), []byte(fork), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Options{
		Path: filepath.Join(root, "usage.sqlite3"), CodexRoots: []string{codexRoot}, Machine: "test-mac",
	})
	defer service.Close()

	dayOne, err := service.Report(context.Background(), ReportOptions{
		Group: "daily", Mode: ModeCalculate,
		Since: time.Date(2026, time.July, 21, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dayOne.Totals.Entries != 1 || dayOne.Totals.Tokens.Input != 750 || dayOne.Totals.Tokens.CacheRead != 250 {
		t.Fatalf("parent day was lost to fork replay: %#v", dayOne.Totals)
	}
	dayTwo, err := service.Report(context.Background(), ReportOptions{
		Group: "session", Mode: ModeCalculate,
		Since: time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dayTwo.Totals.Entries != 1 || dayTwo.Totals.Tokens.Input != 300 || dayTwo.Totals.Tokens.CacheRead != 100 {
		t.Fatalf("fork day includes replayed parent usage: %#v", dayTwo.Totals)
	}
	if len(dayTwo.Rows) != 1 || dayTwo.Rows[0].ProviderSessionID != childID {
		t.Fatalf("fork usage was not attributed to child conversation: %#v", dayTwo.Rows)
	}
	observed, err := service.ObservedSessions(context.Background(),
		time.Date(2026, time.July, 22, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0].ProviderSessionID != childID || len(observed[0].SourcePaths) != 1 ||
		!strings.HasSuffix(observed[0].SourcePaths[0], "rollout-child.jsonl") {
		t.Fatalf("observed fork = %#v", observed)
	}
}

func TestReportRejectsUnknownModeAndMissingTagDimension(t *testing.T) {
	service := NewService(Options{Path: filepath.Join(t.TempDir(), "usage.sqlite3")})
	defer service.Close()
	if _, err := service.Report(context.Background(), ReportOptions{Group: "daily", Mode: "guess"}); err == nil {
		t.Fatal("unknown mode unexpectedly accepted")
	}
	if _, err := service.Report(context.Background(), ReportOptions{Group: "tag", Mode: ModeAuto}); err == nil {
		t.Fatal("tag report without a dimension unexpectedly accepted")
	}
}

func TestSyncRescansSameSizeRewriteAndProfileRoots(t *testing.T) {
	root := t.TempDir()
	runnerRoot := filepath.Join(root, "runners")
	profileRoot := filepath.Join(root, "profiles", "claude", "work")
	projectRoot := filepath.Join(profileRoot, "projects", "project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runnerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(filepath.Join(runnerRoot, "profile-session.json"), state.Metadata{
		ID: "profile-session", Cmd: "claude", Cwd: root, CreatedAt: 1,
		SockPath: filepath.Join(runnerRoot, "profile-session.sock"), ConfigDir: profileRoot,
		ClaudeSessionID: "profile-provider",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectRoot, "profile-provider.jsonl")
	first := `{"timestamp":"2026-07-20T08:00:00Z","sessionId":"profile-provider","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20}}}` + "\n"
	second := strings.Replace(first, `"input_tokens":100`, `"input_tokens":200`, 1)
	if len(first) != len(second) {
		t.Fatal("rewrite fixture must preserve file size")
	}
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Options{Path: filepath.Join(root, "usage.sqlite3"), RunnerStateDir: runnerRoot})
	defer service.Close()
	report, err := service.Report(context.Background(), ReportOptions{Group: "session", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Tokens.Input != 100 || report.Rows[0].SessionID != "profile-session" {
		t.Fatalf("profile report = %#v", report)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	report, err = service.Report(context.Background(), ReportOptions{Group: "session", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Tokens.Input != 200 || report.Totals.Entries != 1 {
		t.Fatalf("rewritten report = %#v", report)
	}
}
