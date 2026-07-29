package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Run explicitly with:
//
//	CLAUDEP_INTEGRATION=1 CGO_ENABLED=0 go test -v ./cmd/sessions -run '^TestClaudePRealTurnsRestartReattach$' -count=1
func TestClaudePRealTurnsRestartReattach(t *testing.T) {
	if os.Getenv("CLAUDEP_INTEGRATION") != "1" {
		t.Skip("set CLAUDEP_INTEGRATION=1 to spend two real Claude subscription turns")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}
	// NewClient removes the variable entirely from every Claude child. Keeping
	// it empty here also proves the test does not depend on a metered API key.
	t.Setenv("ANTHROPIC_API_KEY", "")

	root, err := os.MkdirTemp("/tmp", "sessions-claudep-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runnerBinary := filepath.Join(root, "runner")
	moduleRoot := filepath.Clean(filepath.Join(mustGetwd(t), "..", ".."))
	build := exec.CommandContext(ctx, "go", "build", "-o", runnerBinary, "./cmd/sessions-runner")
	build.Dir = moduleRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, output)
	}

	config := state.Config{
		Host: "127.0.0.1", DefaultShell: "/bin/sh", DefaultCwd: work,
		DefaultCols: 300, DefaultRows: 50,
		StateRoot:       filepath.Join(root, "state"),
		UserStateRoot:   filepath.Join(root, "user-state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		GlobalHooksPath: filepath.Join(root, "hooks.json"),
		RunnerPath:      runnerBinary,
	}
	launcher := newCXWireProcessLauncher(t, runnerBinary, filepath.Join(root, "runner-process.log"))
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		ActivityInterval: 10 * time.Millisecond,
		Notify:           func(sessionruntime.PushPayload) {},
	})
	server := httptest.NewServer(api.New(config, manager, manager.Push()))

	createdOutput := runCXWireCLI(t, server.URL, "--json", "new", "--tool", "claude", "--structured", "--cwd", work)
	var created session
	if err := json.Unmarshal(createdOutput, &created); err != nil {
		t.Fatalf("decode sessions new: %v\n%s", err, createdOutput)
	}
	if created.Kind != state.KindClaudeStructured || created.ClaudeSessionID == "" {
		t.Fatalf("sessions new metadata = %#v", created)
	}
	providerSessionID := created.ClaudeSessionID
	metadata, err := state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Kind != state.KindClaudeStructured || metadata.Info.ClaudeSessionID != providerSessionID {
		t.Fatalf("persisted metadata = %#v", metadata)
	}
	t.Logf("SESSION_ID %s", created.ID)
	t.Logf("CLAUDE_SESSION_ID %s", providerSessionID)
	t.Logf("METADATA kind=%s claudeSessionId=%s", created.Kind, providerSessionID)
	t.Log("AUTH ANTHROPIC_API_KEY absent; Claude subscription login inherited")

	first := sendClaudePTurn(t, server.URL, manager, created.ID, "Reply with exactly CLAUDEP_OK.", "CLAUDEP_OK", 1)
	t.Logf("FIRST_SEND %s", first.send)
	t.Log("FIRST_WORKING_STATE working -> idle")
	firstLast := runCXWireCLI(t, server.URL, "--json", "last", created.ID, "--role", "assistant")
	assertCXWireLast(t, firstLast, "CLAUDEP_OK")
	t.Logf("FIRST_LAST %s", strings.TrimSpace(string(firstLast)))

	secondPrompt := "Reply with exactly CLAUDEP_SECOND_OK if and only if your immediately preceding reply in this same conversation was CLAUDEP_OK; otherwise reply with exactly CLAUDEP_CONTINUITY_FAILED."
	second := sendClaudePTurn(t, server.URL, manager, created.ID, secondPrompt, "CLAUDEP_SECOND_OK", 2)
	t.Logf("SECOND_SEND %s", second.send)
	t.Log("SECOND_WORKING_STATE working -> idle")
	secondLast := runCXWireCLI(t, server.URL, "--json", "last", created.ID, "--role", "assistant")
	assertCXWireLast(t, secondLast, "CLAUDEP_SECOND_OK")
	t.Logf("SECOND_LAST %s", strings.TrimSpace(string(secondLast)))

	current, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("structured Claude session disappeared")
	}
	beforeRestart := current.EventsWindow(nil, nil, nil).Events
	assertClaudePHistorySessionID(t, beforeRestart, providerSessionID)
	if !historyHasClaudePInit(beforeRestart, providerSessionID) {
		t.Fatalf("structured history has no correlated system/init: %s", beforeRestart)
	}
	if countClaudePResultsWithUsage(beforeRestart) < 2 {
		t.Fatalf("structured history has fewer than two result+usage records: %s", beforeRestart)
	}
	beforeCount := len(beforeRestart)

	server.Close()
	manager.Close()
	restarted := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		ActivityInterval: 10 * time.Millisecond,
		Notify:           func(sessionruntime.PushPayload) {},
	})
	defer restarted.Close()
	if err := restarted.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	reattached, ok := restarted.Get(created.ID)
	if !ok {
		t.Fatal("daemon restart did not rediscover structured Claude session")
	}
	if reattached.Info().ClaudeSessionID != providerSessionID {
		t.Fatalf("reattached Claude session = %q, want %q", reattached.Info().ClaudeSessionID, providerSessionID)
	}
	afterRestart := reattached.EventsWindow(nil, nil, nil).Events
	if len(afterRestart) != beforeCount || !historyContainsClaudePAssistant(afterRestart, "CLAUDEP_OK") || !historyContainsClaudePAssistant(afterRestart, "CLAUDEP_SECOND_OK") {
		t.Fatalf("reattached structured history = %s", afterRestart)
	}
	assertClaudePHistorySessionID(t, afterRestart, providerSessionID)
	restartedServer := httptest.NewServer(api.New(config, restarted, restarted.Push()))
	defer restartedServer.Close()
	afterLast := runCXWireCLI(t, restartedServer.URL, "--json", "last", created.ID, "--role", "assistant")
	assertCXWireLast(t, afterLast, "CLAUDEP_SECOND_OK")
	t.Logf("REATTACHED claudeSessionId=%s history=%d last=%s", providerSessionID, len(afterRestart), strings.TrimSpace(string(afterLast)))
	if err := restarted.RequestKill(ctx, created.ID, true); err != nil {
		t.Fatal(err)
	}
}

type claudePTurnObservation struct{ send string }

func sendClaudePTurn(t *testing.T, serverURL string, manager *sessionruntime.Manager, id, prompt, expected string, assistantCount int) claudePTurnObservation {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", serverURL, "--json", "send", id, "--timeout", "60s", prompt}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sessions send: exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	sawWorking := false
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(id)
		if !ok {
			t.Fatal("structured Claude session disappeared during turn")
		}
		if current.Info().Working {
			sawWorking = true
		}
		events := current.EventsWindow(nil, nil, nil).Events
		if sawWorking && !current.Info().Working && countClaudePAssistants(events) >= assistantCount && historyContainsClaudePAssistant(events, expected) {
			return claudePTurnObservation{send: strings.TrimSpace(stdout.String())}
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := manager.Get(id)
	t.Fatalf("Claude turn did not finish: sawWorking=%v info=%#v history=%s", sawWorking, current.Info(), current.EventsWindow(nil, nil, nil).Events)
	return claudePTurnObservation{}
}

func countClaudePAssistants(events []json.RawMessage) int {
	count := 0
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) == nil && event["type"] == "assistant" && event["source"] == claudep.HistorySource {
			count++
		}
	}
	return count
}

func historyContainsClaudePAssistant(events []json.RawMessage, text string) bool {
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil || event["type"] != "assistant" || event["source"] != claudep.HistorySource {
			continue
		}
		message, _ := event["message"].(map[string]any)
		if strings.TrimSpace(codexMessageTextForTest(message["content"])) == text {
			return true
		}
	}
	return false
}

func assertClaudePHistorySessionID(t *testing.T, events []json.RawMessage, want string) {
	t.Helper()
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil || event["source"] != claudep.HistorySource {
			continue
		}
		if sessionID, _ := event["session_id"].(string); sessionID != "" && sessionID != want {
			t.Fatalf("history session_id = %q, want %q: %s", sessionID, want, raw)
		}
	}
}

func historyHasClaudePInit(events []json.RawMessage, sessionID string) bool {
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) == nil && event["source"] == claudep.HistorySource &&
			event["type"] == "system" && event["subtype"] == "init" && event["session_id"] == sessionID {
			return true
		}
	}
	return false
}

func countClaudePResultsWithUsage(events []json.RawMessage) int {
	count := 0
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil || event["source"] != claudep.HistorySource || event["type"] != "result" {
			continue
		}
		usage, _ := event["usage"].(map[string]any)
		positive := false
		for _, value := range usage {
			if number, ok := value.(float64); ok && number > 0 {
				positive = true
				break
			}
		}
		if positive {
			count++
		}
	}
	return count
}
