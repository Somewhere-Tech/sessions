package recap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestGenerateCachesByInputAndProvider(t *testing.T) {
	calls := 0
	service := NewServiceWithRunner(t.TempDir(), func(_ context.Context, settings state.RecapSettings, prompt string) (string, error) {
		calls++
		if settings.Provider != state.RecapProviderCodex {
			t.Fatalf("settings = %#v", settings)
		}
		if !strings.Contains(prompt, "SESSIONS_DAY_JSON") || !strings.Contains(prompt, "Session A") {
			t.Fatalf("prompt = %q", prompt)
		}
		if strings.Contains(prompt, `"id":"private-session-id"`) {
			t.Fatalf("provider prompt leaked a durable session id: %q", prompt)
		}
		return "# Daily recap\n\nWorked on Sessions.", nil
	})
	input := DayInput{Date: "2026-07-22", Timezone: "America/Los_Angeles", Activities: []session.DailyActivity{{ID: "private-session-id", Name: "Session A"}}}
	settings := state.RecapSettings{Provider: "CODEX"}
	first, err := service.Generate(context.Background(), settings, input, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Generate(context.Background(), settings, input, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.InputDigest != second.InputDigest || first.Markdown != second.Markdown {
		t.Fatalf("calls=%d first=%#v second=%#v", calls, first, second)
	}
	if !service.Current(&second, input, state.RecapProviderCodex) {
		t.Fatal("fresh cached document was not current")
	}
	changed := input
	changed.Usage.Entries++
	if service.Current(&second, changed, state.RecapProviderCodex) || service.Current(&second, input, state.RecapProviderClaude) {
		t.Fatal("stale input or provider was accepted as current")
	}
	if _, err := service.Generate(context.Background(), state.DefaultRecapSettings(), input, false); err == nil {
		t.Fatal("off recap unexpectedly generated")
	}
}

func TestDatesListsOnlySavedRecapsNewestFirst(t *testing.T) {
	root := t.TempDir()
	service := NewServiceWithRunner(root, func(_ context.Context, _ state.RecapSettings, _ string) (string, error) {
		return "# Daily recap", nil
	})
	settings := state.RecapSettings{Provider: state.RecapProviderCodex}
	for _, date := range []string{"2026-07-21", "2026-07-23", "2026-07-22"} {
		if _, err := service.Generate(context.Background(), settings, DayInput{Date: date}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "recaps", "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	dates, err := service.Dates()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-07-23", "2026-07-22", "2026-07-21"}
	if !reflect.DeepEqual(dates, want) {
		t.Fatalf("dates = %#v, want %#v", dates, want)
	}
}

func TestProviderInputHasHardByteBoundAndKeepsLatestActivity(t *testing.T) {
	activities := make([]session.DailyActivity, maxPromptActivities)
	for index := range activities {
		activities[index] = session.DailyActivity{
			ID: fmt.Sprintf("secret-id-%d", index), Name: strings.Repeat("activity ", 50),
			Summary: strings.Repeat("large summary ", 80), Tags: map[string]string{"large": strings.Repeat("value ", 80)},
		}
	}
	activities[len(activities)-1].Name = "latest-activity-kept"
	encoded, err := encodeProviderInput(DayInput{Date: "2026-07-22", Activities: activities})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxProviderInputBytes {
		t.Fatalf("provider input = %d bytes, want at most %d", len(encoded), maxProviderInputBytes)
	}
	if len(recapPrompt(encoded)) > maxProviderPromptBytes {
		t.Fatalf("provider prompt = %d bytes, want at most %d", len(recapPrompt(encoded)), maxProviderPromptBytes)
	}
	text := string(encoded)
	if !strings.Contains(text, "latest-activity-kept") || !strings.Contains(text, `"omittedActivities":`) {
		t.Fatalf("bounded provider input did not preserve latest work or omission count: %s", text)
	}
}

func TestProviderInputAliasesHierarchyAndBoundsActivityCount(t *testing.T) {
	activities := make([]session.DailyActivity, maxPromptActivities+2)
	for index := range activities {
		activities[index] = session.DailyActivity{ID: fmt.Sprintf("secret-id-%d", index), Name: "Session", CWD: "/private/project"}
	}
	activities[3].ID = "child-secret-id"
	activities[3].ParentSessionID = activities[2].ID
	encoded, err := encodeProviderInput(DayInput{Date: "2026-07-22", Activities: activities})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{`"id":`, "child-secret-id", `"workdir":"/private/project"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("provider input contains %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"parent":"activity-001"`, `"workdir":"project"`, `"omittedActivities":2`} {
		if !strings.Contains(text, required) {
			t.Fatalf("provider input missing %q: %s", required, text)
		}
	}
}

func TestProviderArgumentsDisableToolsAndPersistence(t *testing.T) {
	claude := strings.Join(providerArguments(state.RecapSettings{Provider: state.RecapProviderClaude}), " ")
	if !strings.Contains(claude, "--effort low") || !strings.Contains(claude, "--tools  --strict-mcp-config") ||
		!strings.Contains(claude, "--safe-mode") || !strings.Contains(claude, "--no-chrome") ||
		!strings.Contains(claude, "--disable-slash-commands") || !strings.Contains(claude, "--no-session-persistence") ||
		strings.Contains(claude, "--model") {
		t.Fatalf("claude args = %q", claude)
	}
	codex := strings.Join(providerArguments(state.RecapSettings{Provider: state.RecapProviderCodex}), " ")
	for _, required := range []string{"--ask-for-approval never", `model_reasoning_effort="low"`, "exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox read-only"} {
		if !strings.Contains(codex, required) {
			t.Fatalf("codex args %q missing %q", codex, required)
		}
	}
	if strings.Contains(codex, "--model") {
		t.Fatalf("codex args hardcode a model: %q", codex)
	}
}

func TestProviderEnvironmentPrefersExistingCLIAuthentication(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "do-not-forward")
	t.Setenv("OPENAI_API_KEY", "do-not-forward")
	t.Setenv("SESSIONS_RECAP_TEST_KEEP", "kept")
	environment := strings.Join(providerEnvironment(), "\n")
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY="} {
		if strings.Contains(environment, forbidden) {
			t.Fatalf("provider environment contains %s", forbidden)
		}
	}
	if !strings.Contains(environment, "SESSIONS_RECAP_TEST_KEEP=kept") {
		t.Fatal("provider environment dropped ordinary variables")
	}
}
