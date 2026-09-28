package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func restartTestDaemon(t *testing.T) startDaemon {
	t.Helper()
	isolation := t.TempDir()
	t.Setenv("HOME", filepath.Join(isolation, "home"))
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(isolation, "runners"))
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(isolation, "lanes.sqlite3"))
	t.Setenv("SESSIONS_PORT", "8899")
	daemon := newTestDaemon(t)
	t.Setenv("HOME", filepath.Join(daemon.root, "home"))
	t.Setenv("SESSIONS_STATE_DIR", daemon.config.RunnerStateDir)
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(daemon.root, "lanes.sqlite3"))
	t.Setenv("SESSIONS_PORT", "8899")
	daemon.config.UserStateRoot = filepath.Join(daemon.root, "user")
	daemon.config.SettingsPath = filepath.Join(daemon.root, "settings.json")
	store, err := ledger.Open(context.Background(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)
	return startDaemon{testDaemon: daemon, manager: manager, store: store}
}

func restartTestSource(t *testing.T, daemon startDaemon) state.SessionInfo {
	t.Helper()
	source, err := daemon.manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--resume", doubleOpenConversation}, ConversationID: doubleOpenConversation,
		Kind: state.KindClaudeStructured, Cwd: daemon.root, Profile: "fixture-work", Name: "Restart fixture", Permissions: state.PermissionsConstrained,
		Claude: &state.ClaudeSessionOptions{Model: "fixture-model", Effort: "high", RemoteControl: state.ClaudeChoiceOff},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source.ConfigDir, "projects", watch.EncodeClaudeCWD(daemon.root), doubleOpenConversation+".jsonl")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"retain this history"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func postRestart(t *testing.T, daemon startDaemon, body restartRequest) (int, restartResult) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/restart", strings.NewReader(string(encoded)), "127.0.0.1:1", nil)
	var result restartResult
	decodeBody(t, response, &result)
	return response.Code, result
}

func TestRestartRetainsConversationAndReplaysWithoutDuplicate(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	request := restartRequest{SourceSessionID: source.ID, ConfirmSessionID: source.ID, Permissions: state.PermissionsFull}
	status, result := postRestart(t, daemon, request)
	if status != 200 || !result.OK || !result.SourceEnded || result.LaneID == "" {
		t.Fatalf("restart=%d %+v", status, result)
	}
	next, found := daemon.manager.Get(result.LaneID)
	if !found {
		t.Fatal("replacement missing")
	}
	info := next.Info()
	if info.Profile != source.Profile || info.ConfigDir != source.ConfigDir || info.Kind != source.Kind || info.Name != source.Name || info.Cwd != source.Cwd || info.ConversationID != source.ConversationID || info.Permissions != state.PermissionsFull {
		t.Fatalf("settings changed: source=%+v next=%+v", source, info)
	}
	args := strings.Join(info.Args, " ")
	if !strings.Contains(args, "--model fixture-model") || !strings.Contains(args, "--effort high") || !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("args=%v", info.Args)
	}
	daemon.handler = New(daemon.config, daemon.manager)
	_, again := postRestart(t, daemon, request)
	if !again.OK || again.LaneID != result.LaneID || len(daemon.launcher.Launches) != 2 {
		t.Fatalf("retry=%+v launches=%d", again, len(daemon.launcher.Launches))
	}
	request.Permissions = state.PermissionsConstrained
	if status, _ := postRestart(t, daemon, request); status != 409 {
		t.Fatalf("changed retry choices=%d", status)
	}
}

func TestRestartPreflightKeepsSourceRunning(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	request := restartRequest{SourceSessionID: source.ID, ConfirmSessionID: "different", Permissions: state.PermissionsFull}
	if status, _ := postRestart(t, daemon, request); status != 400 {
		t.Fatalf("confirmation=%d", status)
	}
	request.ConfirmSessionID = source.ID
	request.RemoteControl = true
	if status, _ := postRestart(t, daemon, request); status != 409 {
		t.Fatalf("consent refusal=%d", status)
	}
	session, _ := daemon.manager.Get(source.ID)
	if session.HasExited() || len(daemon.launcher.Launches) != 1 {
		t.Fatal("preflight ended source")
	}
}

func TestRestartLaunchFailureIsRecoverableWithoutDuplicate(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	request := restartRequest{SourceSessionID: source.ID, ConfirmSessionID: source.ID, Permissions: state.PermissionsConstrained}
	daemon.launcher.Err = errors.New("fixture launch failure")
	status, first := postRestart(t, daemon, request)
	if status != 202 || !first.SourceEnded || first.OK || !strings.Contains(first.Error, "fixture launch failure") {
		t.Fatalf("failure=%d %+v", status, first)
	}
	daemon.launcher.Err = nil
	_, again := postRestart(t, daemon, request)
	if again.OK || again.LaneID == "" || len(daemon.launcher.Launches) != 1 {
		t.Fatalf("retry=%+v launches=%d", again, len(daemon.launcher.Launches))
	}
}

type missingRestartSource struct {
	sessionService
	missingID string
}

func (s missingRestartSource) Get(id string) (*state.Session, bool) {
	if id == s.missingID {
		return nil, false
	}
	return s.sessionService.Get(id)
}

func TestRestartMissingSourceIsNotConfirmedTermination(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	body := restartRequest{SourceSessionID: source.ID, ConfirmSessionID: source.ID, Permissions: state.PermissionsFull}
	if _, err := daemon.handler.prepareRestart(body); err != nil {
		t.Fatal(err)
	}
	daemon.handler.registry = missingRestartSource{sessionService: daemon.manager, missingID: source.ID}
	status, result := postRestart(t, daemon, body)
	live, _ := daemon.manager.Get(source.ID)
	if status != 202 || result.SourceEnded || result.LaneID != "" || result.OK || !strings.Contains(result.Error, "not attached yet") || live.HasExited() || len(daemon.launcher.Launches) != 1 {
		t.Fatalf("missing source restart=%d %+v exited=%v launches=%d", status, result, live.HasExited(), len(daemon.launcher.Launches))
	}
}

func TestRestartRemoteControlKeepsGlobalDefaultsAndUsesTerminal(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	settings := state.Settings{Onboarding: &state.OnboardingSettings{Version: state.OnboardingCurrentVersion, RemoteControlConsent: state.RemoteControlConsentEnabled}, Claude: &state.ClaudeSettings{PermissionMode: state.ClaudePermissionManual, RemoteControl: state.ClaudeChoiceOff}}
	if err := state.SaveSettings(daemon.config.SettingsPath, settings); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(daemon.config.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := restartRequest{SourceSessionID: source.ID, ConfirmSessionID: source.ID, Permissions: state.PermissionsFull, RemoteControl: true}
	status, result := postRestart(t, daemon, body)
	if status != 200 || !result.OK {
		t.Fatalf("remote restart=%d %+v", status, result)
	}
	next, _ := daemon.manager.Get(result.LaneID)
	if next.Info().Kind != "" || !strings.Contains(strings.Join(next.Info().Args, " "), "--remote-control") {
		t.Fatalf("remote replacement=%+v", next.Info())
	}
	after, _ := os.ReadFile(daemon.config.SettingsPath)
	if string(before) != string(after) {
		t.Fatal("restart changed global defaults")
	}
}

func TestRestartAuthenticationFailureLeavesSourceLive(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	encoded, _ := json.Marshal(restartRequest{SourceSessionID: source.ID, ConfirmSessionID: source.ID, Permissions: state.PermissionsFull})
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/restart", strings.NewReader(string(encoded)), "192.0.2.1:1234", http.Header{"Authorization": {"Bearer wrong-fixture-token"}})
	live, _ := daemon.manager.Get(source.ID)
	if response.Code != 401 || live.HasExited() || len(daemon.launcher.Launches) != 1 {
		t.Fatalf("auth=%d exited=%v launches=%d", response.Code, live.HasExited(), len(daemon.launcher.Launches))
	}
}
