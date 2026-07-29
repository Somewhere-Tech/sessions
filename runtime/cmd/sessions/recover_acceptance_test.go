package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	daemonapi "github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestRecoverCLIEndToEndAgainstScratchManager(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	// Excluding launchctl proves this acceptance never inspects or mutates the
	// host's launchd domain. Manager launch is entirely in memory.
	t.Setenv("PATH", t.TempDir())
	ledgerPath := filepath.Join(root, "ledger", "lanes.sqlite3")
	t.Setenv("SESSIONS_LEDGER_PATH", ledgerPath)
	config := cliRecoveryConfig(root)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	providers := map[string]string{
		"closed": "44444444-4444-4444-8444-444444444444",
		"orphan": "55555555-5555-4555-8555-555555555555",
		"live":   "66666666-6666-4666-8666-666666666666",
	}
	created := make(map[string]state.SessionInfo)
	for _, name := range []string{"closed", "orphan", "live"} {
		cliWriteClaudeConversation(t, root, providers[name])
		created[name], err = manager.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "claude", Args: []string{"--resume", providers[name]}, Cwd: root, Name: name,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.RequestKill(context.Background(), created["closed"].ID, false); err != nil {
		t.Fatal(err)
	}
	orphanID := created["orphan"].ID
	if err := os.Remove(filepath.Join(config.RunnerStateDir, orphanID+".json")); err != nil {
		t.Fatal(err)
	}
	launcher.Runner(orphanID).Emit(proto.Event{Kind: proto.EventRunnerLost})
	cliWaitFor(t, func() bool {
		_, present := manager.Get(orphanID)
		return !present && cliHasLedgerEvent(t, store, orphanID, ledger.EventRunnerLost)
	})

	server := httptest.NewServer(daemonapi.New(config, manager, manager.Push()))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "recover"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("recover exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	plan := stdout.String()
	if !strings.Contains(plan, "orphan") || strings.Contains(plan, "\nclosed ") || strings.Contains(plan, "\nlive ") {
		t.Fatalf("unexpected recover plan:\n%s", plan)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "recover", "--reopen"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("recover --reopen exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if len(launcher.Launches) != 4 || !strings.Contains(stdout.String(), "orphan: reopened") {
		t.Fatalf("reopen output=%q launches=%d", stdout.String(), len(launcher.Launches))
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "recover", "--reopen"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("second recover --reopen exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if len(launcher.Launches) != 4 || !strings.Contains(stdout.String(), "no unexpectedly-lost lanes") {
		t.Fatalf("second reopen output=%q launches=%d", stdout.String(), len(launcher.Launches))
	}
	t.Logf("scratch CLI plan_orphan=true first_reopen_launches=%d second_reopen=%q launchctl_path_excluded=true",
		len(launcher.Launches), strings.TrimSpace(stdout.String()))
}

func TestRecoverDefaultIsActionableAndAllExplainsBlockedRows(t *testing.T) {
	actionableID := "30000000-0000-4000-8000-000000000001"
	blockedID := "30000000-0000-4000-8000-000000000002"
	unboundID := "30000000-0000-4000-8000-000000000003"
	report := recovery.Report{
		Lanes: []recovery.Lane{
			{ID: actionableID, Name: "actionable", Tool: "codex", Cwd: "/work", Class: ledger.ClassUnexpectedlyLost},
			{ID: blockedID, Name: "stale source", Tool: "codex", Cwd: "/work", Class: ledger.ClassUnexpectedlyLost, Anomalies: []ledger.Anomaly{ledger.AnomalyResumeSourceMissing}},
			{ID: unboundID, Name: "unbound", Tool: "codex", Cwd: "/work", Class: ledger.ClassUnexpectedlyLost, Anomalies: []ledger.Anomaly{ledger.AnomalyProviderUnbound}},
		},
		Plan: ledger.RecoveryPlan{Recipes: []ledger.RecoveryRecipe{
			{SourceLaneID: actionableID, Cmd: "codex", Args: []string{"resume", "conversation"}},
			{SourceLaneID: blockedID, Cmd: "codex", Args: []string{"resume", "missing"}, Blocked: true},
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/recovery" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(report)
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "recover")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "RESUME") || !strings.Contains(stdout, "actionable") ||
		strings.Contains(stdout, "stale source") || strings.Contains(stdout, "unbound") || strings.Contains(stdout, "provider-unbound") {
		t.Fatalf("recover default exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "recover", "--all")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "REASON") || strings.Contains(stdout, "\tRESUME") ||
		!strings.Contains(stdout, "actionable") || !strings.Contains(stdout, "blocked") ||
		!strings.Contains(stdout, "provider-unbound") || !strings.Contains(stdout, "stale or missing") {
		t.Fatalf("recover --all exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "recover")
	if code != 0 || stderr != "" {
		t.Fatalf("recover --json exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var decoded struct {
		Lanes []struct {
			Status string `json:"status"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Lanes) != 3 || decoded.Lanes[0].Status != "actionable" || decoded.Lanes[1].Status != "blocked" || decoded.Lanes[2].Status != "provider-unbound" {
		t.Fatalf("recover JSON statuses = %+v", decoded.Lanes)
	}
}

func TestAdoptCLIExplicitlyBindsScratchCodexConversation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", t.TempDir())
	ledgerPath := filepath.Join(root, "ledger", "lanes.sqlite3")
	t.Setenv("SESSIONS_LEDGER_PATH", ledgerPath)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	config := cliRecoveryConfig(root)
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	provider := "77777777-7777-4777-8777-777777777777"
	rollout := filepath.Join(root, ".codex", "sessions", "2026", "07", "16", "rollout-2026-07-16T12-00-00-"+provider+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"type": "session_meta", "payload": map[string]any{
			"id": provider, "cwd": root, "timestamp": "2026-07-16T12:00:00Z",
		},
	})
	if err := os.WriteFile(rollout, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(daemonapi.New(config, manager, manager.Push()))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "adopt", rollout}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("adopt exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	laneID := strings.TrimSpace(stdout.String())
	if laneID == "" || len(launcher.Launches) != 1 {
		t.Fatalf("adopt lane=%q launches=%d", laneID, len(launcher.Launches))
	}
	events, err := store.Events(context.Background(), laneID)
	if err != nil {
		t.Fatal(err)
	}
	actorCreated, actorBound := false, false
	for _, event := range events {
		if event.Actor == ledger.ActorAdopt && event.Type == ledger.EventCreated {
			actorCreated = true
		}
		if event.Actor == ledger.ActorAdopt && event.Type == ledger.EventProviderBound {
			actorBound = true
		}
	}
	if !actorCreated || !actorBound {
		t.Fatalf("adopt actor facts missing: %+v", events)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "adopt", provider}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("duplicate adopt exit=%d stderr=%q", code, stderr.String())
	}
	if len(launcher.Launches) != 1 || !strings.Contains(stderr.String(), "conversation "+provider+" is already live") ||
		!strings.Contains(stderr.String(), "sessions attach "+laneID) || !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("duplicate adopt launches=%d stderr=%q", len(launcher.Launches), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "adopt", provider, "--force"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("adopt --force exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	forcedID := strings.TrimSpace(stdout.String())
	if len(launcher.Launches) != 2 || forcedID == "" || !cliHasLedgerEvent(t, store, laneID, ledger.EventProviderRebound) {
		t.Fatalf("forced adopt lane=%q launches=%d rebound=%t", forcedID, len(launcher.Launches), cliHasLedgerEvent(t, store, laneID, ledger.EventProviderRebound))
	}
	if original, present := manager.Get(laneID); !present || original.Info().Exited {
		t.Fatal("adopt --force killed the original session")
	}
	t.Logf("scratch adopt lane=%s forced=%s launches=%d actor_created=%t actor_bound=%t duplicate_refused=true original_live=true rebound=true",
		laneID, forcedID, len(launcher.Launches), actorCreated, actorBound)
}

func cliRecoveryConfig(root string) state.Config {
	return state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"),
		TokenPath:      filepath.Join(root, "state", "token"), OpenPath: filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"), GlobalHooksPath: filepath.Join(root, "hooks.json"),
		RunnerPath: "/scratch/fake-runner",
	}
}

func cliWriteClaudeConversation(t *testing.T, root, provider string) {
	t.Helper()
	dir := filepath.Join(root, ".claude", "projects", watch.EncodeClaudeCWD(root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "cwd": root, "sessionId": provider})
	if err := os.WriteFile(filepath.Join(dir, provider+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliHasLedgerEvent(t *testing.T, store *ledger.Store, laneID string, kind ledger.EventType) bool {
	t.Helper()
	events, err := store.Events(context.Background(), laneID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == kind {
			return true
		}
	}
	return false
}

func cliWaitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-t.Context().Done():
			t.Fatal("test ended before scratch recovery state arrived")
		default:
			runtime.Gosched()
		}
	}
}
