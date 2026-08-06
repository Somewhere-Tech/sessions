package usage

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const codexTierTurn = `{"type":"codex","subtype":"token_count","source":"codex-app-server","timestamp":"2026-07-20T09:00:02Z","conversationId":"codex-tier","turnId":"turn-1","usage":{"last":{"inputTokens":1000,"cachedInputTokens":250,"outputTokens":125,"reasoningOutputTokens":25}}}`

const codexTierLog = `{"timestamp":"2026-07-20T09:00:00Z","type":"session_meta","payload":{"id":"codex-tier"}}
{"timestamp":"2026-07-20T09:00:01Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5.3-codex"}}
{"timestamp":"2026-07-20T09:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":250,"output_tokens":125,"reasoning_output_tokens":25}}}}
`

// gpt-5.3-codex costs 1.75 / 14 / 0.175 per million and doubles on the
// priority tier; the fixture turn bills 750 input, 125 output, 250 cache read.
const (
	standardTierCost = (750*1.75 + 125*14 + 250*.175) / 1_000_000
	priorityTierCost = 2 * standardTierCost
)

type codexTierFixture struct {
	root      string
	codexHome string
	runners   string
	ledgers   int
}

// newCodexTierFixture lays out one Codex session on disk: a rollout log, the
// machine config, and the runner metadata describing how it was launched.
func newCodexTierFixture(t *testing.T, launchArgs []string, configTOML string) *codexTierFixture {
	t.Helper()
	fixture := &codexTierFixture{root: t.TempDir()}
	fixture.codexHome = filepath.Join(fixture.root, ".codex")
	fixture.runners = filepath.Join(fixture.root, "runners")
	sessionsRoot := filepath.Join(fixture.codexHome, "sessions", "2026", "07", "20")
	for _, dir := range []string{sessionsRoot, fixture.runners} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if configTOML != "" {
		if err := os.WriteFile(filepath.Join(fixture.codexHome, "config.toml"), []byte(configTOML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sessionsRoot, "rollout.jsonl"), []byte(codexTierLog), 0o600); err != nil {
		t.Fatal(err)
	}
	if launchArgs != nil {
		if err := state.WriteMetadata(filepath.Join(fixture.runners, "sessions-codex.json"), state.Metadata{
			ID: "sessions-codex", Cmd: "codex", Args: launchArgs, Cwd: fixture.root, CreatedAt: 1,
			SockPath: filepath.Join(fixture.runners, "sessions-codex.sock"), ConversationID: "codex-tier",
			ConfigDir: fixture.codexHome,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// service returns a usage service over a ledger of its own, so each caller can
// exercise a different order of writers against the same on-disk session.
func (f *codexTierFixture) service(t *testing.T) *Service {
	t.Helper()
	f.ledgers++
	service := NewService(Options{
		Path:           filepath.Join(f.root, "ledger", strconv.Itoa(f.ledgers), "usage.sqlite3"),
		CodexRoots:     []string{filepath.Join(f.codexHome, "sessions")},
		RunnerStateDir: f.runners,
		Machine:        "test-mac",
	})
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func (f *codexTierFixture) liveInfo(args []string, fast bool) state.SessionInfo {
	return state.SessionInfo{
		ID: "sessions-codex", Tool: state.ToolCodex, ConversationID: "codex-tier",
		Model: "gpt-5.3-codex", Args: args, ConfigDir: f.codexHome, Fast: fast,
	}
}

func codexTierCost(t *testing.T, service *Service) float64 {
	t.Helper()
	report, err := service.Report(context.Background(), ReportOptions{Group: "session", Mode: ModeCalculate})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Entries != 1 {
		t.Fatalf("expected exactly one merged event, got %#v", report.Totals)
	}
	return report.Totals.CalculatedCostUSD
}

func assertCost(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s cost = %.12f, want %.12f", name, got, want)
	}
}

// The live recorder and the provider-log scanner write the same event_key, so
// the cost recorded for one session must not depend on which of them got there
// first, nor on whether a rescan happened afterwards.
func TestLiveAndBackfillPriceTheSameSessionIdentically(t *testing.T) {
	launch := []string{"-c", `service_tier="priority"`, "-m", "gpt-5.3-codex"}
	for _, config := range []string{"", "service_tier = \"priority\"\n", "service_tier = \"flex\"\n"} {
		t.Run("config "+strconv.Quote(config), func(t *testing.T) {
			fixture := newCodexTierFixture(t, launch, config)
			info := fixture.liveInfo(launch, true)

			scanOnly := fixture.service(t)
			assertCost(t, "scan only", codexTierCost(t, scanOnly), priorityTierCost)

			liveFirst := fixture.service(t)
			if err := liveFirst.RecordStructured(context.Background(), info, json.RawMessage(codexTierTurn)); err != nil {
				t.Fatal(err)
			}
			// Report syncs first, so this is the live write followed by backfill.
			assertCost(t, "live then backfill", codexTierCost(t, liveFirst), priorityTierCost)

			scanFirst := fixture.service(t)
			if _, err := scanFirst.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := scanFirst.RecordStructured(context.Background(), info, json.RawMessage(codexTierTurn)); err != nil {
				t.Fatal(err)
			}
			assertCost(t, "backfill then live", codexTierCost(t, scanFirst), priorityTierCost)
		})
	}
}

// A tier set in config.toml is the machine default and must reach the live
// recorder too: it used to price from the session's flags alone while the
// scanner priced from the file alone.
func TestConfigDefaultTierReachesLiveAndBackfillAlike(t *testing.T) {
	launch := []string{"-m", "gpt-5.3-codex"}
	fixture := newCodexTierFixture(t, launch, "service_tier = \"priority\"\n")

	live := fixture.service(t)
	if err := live.RecordStructured(context.Background(), fixture.liveInfo(launch, false), json.RawMessage(codexTierTurn)); err != nil {
		t.Fatal(err)
	}
	db, err := live.database(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var liveCost float64
	if err := db.QueryRow(`SELECT calculated_cost_usd FROM usage_entries`).Scan(&liveCost); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "live", liveCost, priorityTierCost)
	assertCost(t, "backfill", codexTierCost(t, fixture.service(t)), priorityTierCost)
}

// One session's launch flags decide that session's price and outrank the
// machine default.
func TestSessionLaunchTierOutranksMachineDefault(t *testing.T) {
	standard := newCodexTierFixture(t, []string{"-c", `service_tier="flex"`}, "service_tier = \"priority\"\n")
	assertCost(t, "session on the standard tier", codexTierCost(t, standard.service(t)), standardTierCost)

	unlaunched := newCodexTierFixture(t, nil, "service_tier = \"priority\"\n")
	assertCost(t, "session with no launch evidence", codexTierCost(t, unlaunched.service(t)), priorityTierCost)
}

// Once a session has been priced from its own launch flags, losing that
// evidence or editing the machine config must not silently reprice history.
func TestDecidedSessionTierSurvivesConfigEdits(t *testing.T) {
	fixture := newCodexTierFixture(t, []string{"-c", `service_tier="priority"`}, "")
	service := fixture.service(t)
	assertCost(t, "first scan", codexTierCost(t, service), priorityTierCost)

	if err := os.RemoveAll(fixture.runners); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.codexHome, "config.toml"), []byte("service_tier = \"flex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "rescan after the evidence went away", codexTierCost(t, service), priorityTierCost)
}

// A profile prices the sessions that select it, and only those.
func TestLaunchProfileSelectsThatProfilesTier(t *testing.T) {
	config := "service_tier = \"flex\"\n\n[profiles.fastlane]\nservice_tier = \"priority\"\n"
	profiled := newCodexTierFixture(t, []string{"--profile", "fastlane"}, config)
	assertCost(t, "session on the fastlane profile", codexTierCost(t, profiled.service(t)), priorityTierCost)

	plain := newCodexTierFixture(t, []string{"-m", "gpt-5.3-codex"}, config)
	assertCost(t, "session on no profile", codexTierCost(t, plain.service(t)), standardTierCost)
}

// When the provider log takes over a row the live recorder wrote, the tokens,
// the model and the price must all move together. Taking the log's model while
// keeping the live price left rows describing one writer's usage at another
// writer's rate.
func TestBackfillReplacesLiveModelAndCostTogether(t *testing.T) {
	launch := []string{"-c", `service_tier="priority"`}
	fixture := newCodexTierFixture(t, launch, "")
	service := fixture.service(t)

	info := fixture.liveInfo(launch, true)
	info.Model = "gpt-5.4" // the live session reports a different model than the log
	if err := service.RecordStructured(context.Background(), info, json.RawMessage(codexTierTurn)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := service.database(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var model string
	var cost float64
	if err := db.QueryRow(`SELECT model, calculated_cost_usd FROM usage_entries`).Scan(&model, &cost); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.3-codex" {
		t.Fatalf("provider log did not win the model: %q", model)
	}
	assertCost(t, "backfilled row priced at its own model", cost, priorityTierCost)
}

func TestSessionBindingsReadKnownProviderSessionSpellings(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		claude bool
		want   string
	}{
		{name: "session id", args: []string{"--session-id", "abc"}, claude: true, want: "abc"},
		{name: "resume", args: []string{"--resume", "abc"}, claude: true, want: "abc"},
		{name: "session id joined", args: []string{"--session-id=abc"}, claude: true, want: "abc"},
		{name: "resume joined", args: []string{"--resume=abc"}, claude: true, want: "abc"},
		{name: "claude resume shorthand", args: []string{"-r", "abc"}, claude: true, want: "abc"},
		{name: "codex ignores the shorthand", args: []string{"-r", "abc"}},
		{name: "codex resume", args: []string{"resume", "--resume", "abc"}, want: "abc"},
		{name: "trailing flag", args: []string{"--resume"}, claude: true},
		{name: "no identity", args: []string{"--model", "sonnet"}, claude: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := argvProviderSession(test.args, test.claude); got != test.want {
				t.Fatalf("argvProviderSession(%v) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func TestLaunchServiceTierReadsKnownSpellings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[profiles.cheap]\nservice_tier = \"flex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		args  []string
		tier  string
		found bool
	}{
		{name: "config flag", args: []string{"-c", `service_tier="priority"`}, tier: "priority", found: true},
		{name: "long config flag", args: []string{"--config", "service_tier=fast"}, tier: "fast", found: true},
		{name: "profile flag", args: []string{"--profile", "cheap"}, tier: "flex", found: true},
		{name: "profile shorthand", args: []string{"-p", "cheap"}, tier: "flex", found: true},
		{name: "profile via config flag", args: []string{"-c", "profile=cheap"}, tier: "flex", found: true},
		{name: "unknown profile is not evidence", args: []string{"-p", "missing"}},
		{name: "no tier evidence", args: []string{"-m", "gpt-5.3-codex"}},
		{name: "trailing flag", args: []string{"-c"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tier, found := launchServiceTier(test.args, dir)
			if tier != test.tier || found != test.found {
				t.Fatalf("launchServiceTier = %q %v, want %q %v", tier, found, test.tier, test.found)
			}
		})
	}
}
