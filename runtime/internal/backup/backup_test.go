package backup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const fixtureToken = "smt_fixture-token-never-real"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestEnableStoresTokenPathWithPrivateMode(t *testing.T) {
	home := t.TempDir()
	tokenPath := SomewhereConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"profile":{"token":"`+fixtureToken+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := ConfigPath(home)
	config, err := Enable(path, tokenPath, "fixture-project", 9*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Project != "fixture-project" || config.Interval != "9m0s" {
		t.Fatalf("config = %#v", config)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fixtureToken) {
		t.Fatal("Sessions config copied the somewhere token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
}

func TestPushRawTranscriptManifestAndIncrementalSkip(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	projectsDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, projectsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "11111111-2222-4333-8444-555555555555"
	conversation := []byte("{\"type\":\"user\",\"message\":\"fixture bytes\"}\n")
	conversationPath := filepath.Join(projectsDir, watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conversationPath, conversation, 0o600); err != nil {
		t.Fatal(err)
	}

	tokenPath := filepath.Join(root, "somewhere.json")
	if err := os.WriteFile(tokenPath, []byte(`{"token":"`+fixtureToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	if err := SaveConfig(configPath, Config{
		Enabled: true, Project: "fixture-project", TokenPath: tokenPath,
		Interval: "15m", Cache: make(map[string]Fingerprint),
	}); err != nil {
		t.Fatal(err)
	}

	type upload struct {
		method      string
		path        string
		authorize   string
		contentType string
		body        []byte
	}
	var mu sync.Mutex
	var uploads []upload
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		uploads = append(uploads, upload{
			method: request.Method, path: request.URL.Path,
			authorize:   request.Header.Get("Authorization"),
			contentType: request.Header.Get("Content-Type"), body: body,
		})
		mu.Unlock()
		response.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	now := time.Date(2026, time.July, 16, 17, 0, 0, 0, time.UTC)
	pusher := NewPusher(Options{
		ConfigPath: configPath, RunnerStateDir: runnerDir,
		ClaudeProjectsDir: projectsDir, Machine: "fixture-mac",
		APIBase: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	live := []state.SessionInfo{{
		ID: id, Name: "fixture session", Cmd: "claude", Args: []string{"--session-id", id},
		Cwd: cwd, Tool: state.ToolClaude, CreatedAt: now.Add(-time.Hour).UnixMilli(),
		LastDataAt: now.Add(-time.Minute).UnixMilli(),
	}}
	first, err := pusher.Push(t.Context(), live)
	if err != nil {
		t.Fatal(err)
	}
	if first.Uploaded != 1 || first.Skipped != 0 || first.SessionCount != 1 {
		t.Fatalf("first result = %#v", first)
	}
	mu.Lock()
	firstUploads := append([]upload(nil), uploads...)
	mu.Unlock()
	if len(firstUploads) != 2 {
		t.Fatalf("uploads = %d, want transcript + manifest", len(firstUploads))
	}
	wantTranscriptPath := "/v1/fs/fixture-project/sessions/fixture-mac/claude/" + id + ".jsonl"
	if firstUploads[0].method != http.MethodPut || firstUploads[0].path != wantTranscriptPath ||
		firstUploads[0].authorize != "Bearer "+fixtureToken ||
		firstUploads[0].contentType != "application/octet-stream" ||
		!reflect.DeepEqual(firstUploads[0].body, conversation) {
		t.Fatalf("transcript upload = %#v", firstUploads[0])
	}
	if firstUploads[1].method != http.MethodPut || firstUploads[1].path != "/v1/fs/fixture-project/sessions/fixture-mac/manifest.json" {
		t.Fatalf("manifest request = %#v", firstUploads[1])
	}
	var manifest Manifest
	if err := json.Unmarshal(firstUploads[1].body, &manifest); err != nil {
		t.Fatal(err)
	}
	entry, ok := manifest.Sessions[id]
	if !ok || entry.Name != "fixture session" || entry.CWD != cwd || entry.Tool != "claude" ||
		entry.Path != strings.TrimPrefix(wantTranscriptPath, "/v1/fs/fixture-project/") {
		t.Fatalf("manifest = %#v", manifest)
	}
	if strings.Contains(string(firstUploads[1].body), fixtureToken) || strings.Contains(string(firstUploads[1].body), "--session-id") {
		t.Fatal("manifest contains credential or process arguments")
	}

	second, err := pusher.Push(t.Context(), live)
	if err != nil {
		t.Fatal(err)
	}
	if second.Uploaded != 0 || second.Skipped != 1 || second.SessionCount != 1 {
		t.Fatalf("second result = %#v", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(uploads) != 3 || uploads[2].path != "/v1/fs/fixture-project/sessions/fixture-mac/manifest.json" {
		t.Fatalf("incremental uploads = %#v", uploads)
	}
}

func TestCollectKnownSessionsAndHonorOptOutFlags(t *testing.T) {
	runnerDir := t.TempDir()
	writeMetadata := func(id string, backupValue *bool) {
		t.Helper()
		path := filepath.Join(runnerDir, id+".json")
		if err := state.WriteMetadata(path, state.Metadata{
			ID: id, Name: id, Cmd: "claude", Args: []string{"--session-id", id},
			Cwd: t.TempDir(), CreatedAt: time.Now().Add(-time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		if backupValue == nil {
			return
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		value["backup"] = *backupValue
		encoded, _ = json.Marshal(value)
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	included := "aaaaaaaa-1111-4222-8333-444444444444"
	excluded := "bbbbbbbb-1111-4222-8333-444444444444"
	writeMetadata(included, nil)
	no := false
	writeMetadata(excluded, &no)
	sessions := CollectSessions(nil, runnerDir)
	if len(sessions) != 2 || sessions[0].ID != included || sessions[0].OptOut || sessions[1].ID != excluded || !sessions[1].OptOut {
		t.Fatalf("sessions = %#v", sessions)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, included+".no-backup"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sessions = CollectSessions(nil, runnerDir)
	if !sessions[0].OptOut {
		t.Fatal("no-backup sentinel was ignored")
	}
}

func TestResolverFindsCodexRollout(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	rollout := filepath.Join(root, "2026", "07", "16", "rollout-fixture.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"timestamp": now.Format(time.RFC3339Nano), "type": "session_meta",
		"payload": map[string]any{"cwd": cwd, "timestamp": now.Format(time.RFC3339Nano)},
	}
	encoded, _ := json.Marshal(record)
	if err := os.WriteFile(rollout, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, tool := (Resolver{CodexSessionsDir: root, Now: func() time.Time { return now }}).Resolve(Session{
		ID: "codex-fixture", CWD: cwd, Tool: state.ToolCodex,
		CreatedAt: now.Add(-time.Minute).UnixMilli(),
	})
	if resolved != rollout || tool != "codex" {
		t.Fatalf("resolved=%q tool=%q", resolved, tool)
	}
}

func TestResolverUsesSessionProfileConfigDirBeforeGlobalRoots(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	configDir := filepath.Join(root, "profiles", "codex", "work")
	rollout := filepath.Join(configDir, "sessions", "2026", "07", "19", "rollout-profile.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"timestamp": now.Format(time.RFC3339Nano), "type": "session_meta",
		"payload": map[string]any{"cwd": cwd, "timestamp": now.Format(time.RFC3339Nano)},
	}
	encoded, _ := json.Marshal(record)
	if err := os.WriteFile(rollout, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, tool := (Resolver{
		CodexSessionsDir: filepath.Join(root, "wrong-default"), Now: func() time.Time { return now },
	}).Resolve(Session{
		ID: "profile-codex", CWD: cwd, ConfigDir: configDir, Tool: state.ToolCodex,
		CreatedAt: now.Add(-time.Minute).UnixMilli(),
	})
	if resolved != rollout || tool != "codex" {
		t.Fatalf("profile resolved=%q tool=%q, want %q codex", resolved, tool, rollout)
	}
}

func TestPeriodicServiceRunsOnlyWhenEnabled(t *testing.T) {
	root := t.TempDir()
	tokenPath := filepath.Join(root, "somewhere.json")
	if err := os.WriteFile(tokenPath, []byte(`{"token":"`+fixtureToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	config := Config{
		Project: "periodic-fixture", TokenPath: tokenPath, Interval: "10ms",
	}
	if err := SaveConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		select {
		case requests <- struct{}{}:
		default:
		}
		response.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	service := NewService(Options{
		ConfigPath: configPath, RunnerStateDir: filepath.Join(root, "runners"),
		Machine: "periodic-mac", APIBase: server.URL, HTTPClient: server.Client(),
	}, nil)
	defer service.Close()
	if err := service.ReloadPeriodic(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
		t.Fatal("disabled backup started a periodic upload")
	case <-time.After(30 * time.Millisecond):
	}
	config.Enabled = true
	if err := SaveConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	if err := service.ReloadPeriodic(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("enabled backup did not run its periodic upload")
	}
}

func TestPeriodicServiceCloseCancelsAndWaitsForInFlightPush(t *testing.T) {
	root := t.TempDir()
	tokenPath := filepath.Join(root, "somewhere.json")
	if err := os.WriteFile(tokenPath, []byte(`{"token":"`+fixtureToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	if err := SaveConfig(configPath, Config{
		Enabled: true, Project: "close-fixture", TokenPath: tokenPath, Interval: "1ms",
	}); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	releaseRequest := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestStarted)
		select {
		case <-request.Context().Done():
			close(requestCanceled)
			return nil, request.Context().Err()
		case <-releaseRequest:
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}
	})}
	t.Cleanup(func() { close(releaseRequest) })
	service := NewService(Options{
		ConfigPath: configPath, RunnerStateDir: filepath.Join(root, "runners"),
		Machine: "close-mac", APIBase: "https://backup.invalid", HTTPClient: client,
	}, nil)
	t.Cleanup(service.Close)
	if err := service.ReloadPeriodic(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		service.Close()
		t.Fatal("periodic upload did not reach the server")
	}

	service.Close()
	select {
	case <-requestCanceled:
	default:
		t.Fatal("Close returned before the in-flight periodic upload was canceled")
	}
}
