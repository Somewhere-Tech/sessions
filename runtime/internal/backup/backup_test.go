package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// pushFixture builds a Claude backup fixture with one transcript per session
// id. Ids are pushed in ascending order, so an early failure used to strand
// every later session.
type pushFixture struct {
	configPath  string
	transcripts map[string]string
	pusher      *Pusher
	live        []state.SessionInfo
	now         time.Time
}

func newPushFixture(t *testing.T, handler http.HandlerFunc, ids ...string) *pushFixture {
	t.Helper()
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	projectsDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "worktree")
	for _, dir := range []string{runnerDir, projectsDir, cwd} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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

	fixture := &pushFixture{
		configPath:  configPath,
		transcripts: make(map[string]string),
		now:         time.Date(2026, time.July, 16, 17, 0, 0, 0, time.UTC),
	}
	for _, id := range ids {
		path := filepath.Join(projectsDir, watch.EncodeClaudeCWD(cwd), id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"user","message":"`+id+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.transcripts[id] = path
		fixture.live = append(fixture.live, state.SessionInfo{
			ID: id, Name: "session " + id, Cmd: "claude", Args: []string{"--session-id", id},
			Cwd: cwd, Tool: state.ToolClaude, CreatedAt: fixture.now.Add(-time.Hour).UnixMilli(),
			LastDataAt: fixture.now.Add(-time.Minute).UnixMilli(),
		})
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	fixture.pusher = NewPusher(Options{
		ConfigPath: configPath, RunnerStateDir: runnerDir,
		ClaudeProjectsDir: projectsDir, Machine: "fixture-mac",
		APIBase: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return fixture.now },
	})
	return fixture
}

func (f *pushFixture) config(t *testing.T) Config {
	t.Helper()
	config, err := LoadConfig(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func remotePathFor(id string) string {
	return "sessions/fixture-mac/claude/" + id + ".jsonl"
}

// A single failing session must not strand the sessions sorted after it, and
// must not stop the manifest from being written. The scheduled push runs every
// 15 minutes, so aborting the run repeats the loss until the session ends.
func TestPushSkipsOneFailedSessionAndCompletesTheRun(t *testing.T) {
	busy := "11111111-1111-4111-8111-111111111111"
	healthy := "22222222-2222-4222-8222-222222222222"
	var mu sync.Mutex
	var received []string
	var refuse atomic.Bool
	refuse.Store(true)
	fixture := newPushFixture(t, func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if strings.HasSuffix(request.URL.Path, busy+".jsonl") && refuse.Load() {
			http.Error(response, "session is busy", http.StatusServiceUnavailable)
			return
		}
		mu.Lock()
		received = append(received, request.URL.Path)
		mu.Unlock()
		response.WriteHeader(http.StatusCreated)
	}, busy, healthy)

	first, err := fixture.pusher.Push(t.Context(), fixture.live)
	if err != nil {
		t.Fatalf("Push abandoned the run for one failed session: %v", err)
	}
	if first.Uploaded != 1 || first.Unresolved != 1 || first.SessionCount != 1 {
		t.Fatalf("first result = %#v", first)
	}
	if len(first.UnresolvedSessions) != 1 || first.UnresolvedSessions[0].ID != busy ||
		!strings.Contains(first.UnresolvedSessions[0].Reason, "503") {
		t.Fatalf("unresolved sessions = %#v, want the busy session and why", first.UnresolvedSessions)
	}
	if first.ManifestPath == "" {
		t.Fatal("Push left the manifest unwritten after one failed session")
	}
	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if len(got) != 2 || !strings.HasSuffix(got[0], healthy+".jsonl") ||
		!strings.HasSuffix(got[1], "manifest.json") {
		t.Fatalf("uploads = %#v, want the later session and the manifest", got)
	}

	config := fixture.config(t)
	if _, ok := config.Cache["fixture-project/"+remotePathFor(busy)]; ok {
		t.Fatal("the incremental cache marked a skipped transcript as uploaded")
	}
	if _, ok := config.Cache["fixture-project/"+remotePathFor(healthy)]; !ok {
		t.Fatal("the incremental cache forgot the transcript it did upload")
	}
	if config.LastPushPending != 1 {
		t.Fatalf("LastPushPending = %d, want the partial push recorded", config.LastPushPending)
	}

	// The next scheduled push retries only the session that was left behind.
	refuse.Store(false)
	second, err := fixture.pusher.Push(t.Context(), fixture.live)
	if err != nil {
		t.Fatal(err)
	}
	if second.Uploaded != 1 || second.Skipped != 1 || second.Unresolved != 0 || second.SessionCount != 2 {
		t.Fatalf("second result = %#v", second)
	}
	if len(second.UnresolvedSessions) != 0 {
		t.Fatalf("second unresolved sessions = %#v", second.UnresolvedSessions)
	}
}

// The manifest may not advertise a transcript that has never been uploaded,
// and must keep advertising one an earlier push already placed remotely.
func TestPushManifestOnlyClaimsTranscriptsThatExistRemotely(t *testing.T) {
	stable := "33333333-3333-4333-8333-333333333333"
	flaky := "44444444-4444-4444-8444-444444444444"
	var mu sync.Mutex
	manifests := make([][]byte, 0, 2)
	var refuse atomic.Bool
	fixture := newPushFixture(t, func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if strings.HasSuffix(request.URL.Path, flaky+".jsonl") && refuse.Load() {
			http.Error(response, "session is busy", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(request.URL.Path, "manifest.json") {
			mu.Lock()
			manifests = append(manifests, body)
			mu.Unlock()
		}
		response.WriteHeader(http.StatusCreated)
	}, stable, flaky)

	if _, err := fixture.pusher.Push(t.Context(), fixture.live); err != nil {
		t.Fatal(err)
	}
	// Now the transcript exists remotely but a later push cannot refresh it.
	refuse.Store(true)
	if err := os.WriteFile(fixture.transcripts[flaky], []byte(`{"type":"user","message":"more"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.pusher.Push(t.Context(), fixture.live)
	if err != nil {
		t.Fatal(err)
	}
	if result.Unresolved != 1 || result.SessionCount != 2 {
		t.Fatalf("result = %#v, want the stale entry kept and the failure reported", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(manifests) != 2 {
		t.Fatalf("manifests = %d, want one per push", len(manifests))
	}
	for index, encoded := range manifests {
		var manifest Manifest
		if err := json.Unmarshal(encoded, &manifest); err != nil {
			t.Fatal(err)
		}
		if _, ok := manifest.Sessions[stable]; !ok {
			t.Fatalf("manifest %d dropped the healthy session", index)
		}
		if _, ok := manifest.Sessions[flaky]; !ok {
			t.Fatalf("manifest %d dropped a session that exists remotely", index)
		}
	}
}

func TestPushDoesNotClaimASessionItNeverUploaded(t *testing.T) {
	unreadable := "55555555-5555-4555-8555-555555555555"
	healthy := "66666666-6666-4666-8666-666666666666"
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 transcript")
	}
	fixture := newPushFixture(t, func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		response.WriteHeader(http.StatusCreated)
	}, unreadable, healthy)
	if err := os.Chmod(fixture.transcripts[unreadable], 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.transcripts[unreadable], 0o600) })

	result, err := fixture.pusher.Push(t.Context(), fixture.live)
	if err != nil {
		t.Fatalf("Push abandoned the run for one unreadable transcript: %v", err)
	}
	if result.Uploaded != 1 || result.Unresolved != 1 || result.SessionCount != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(result.UnresolvedSessions) != 1 || result.UnresolvedSessions[0].ID != unreadable {
		t.Fatalf("unresolved sessions = %#v", result.UnresolvedSessions)
	}
}

func TestPushStopsWhenTheCallerCancels(t *testing.T) {
	first := "77777777-7777-4777-8777-777777777777"
	second := "88888888-8888-4888-8888-888888888888"
	ctx, cancel := context.WithCancel(t.Context())
	var uploads atomic.Int32
	fixture := newPushFixture(t, func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		uploads.Add(1)
		cancel()
		response.WriteHeader(http.StatusCreated)
	}, first, second)

	if _, err := fixture.pusher.Push(ctx, fixture.live); !errors.Is(err, context.Canceled) {
		t.Fatalf("Push = %v, want the cancellation surfaced", err)
	}
	if got := uploads.Load(); got != 1 {
		t.Fatalf("uploads = %d, want the run to stop at the cancellation", got)
	}
}

// A live transcript is the routine cause of a skipped session, so the reason
// has to read as a retry rather than a failure.
func TestReadStableFileReportsAGrowingTranscriptCalmly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes are POSIX here")
	}
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo is unavailable: " + err.Error())
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// readStableFile makes two attempts; feed both.
		for attempt := 0; attempt < 2; attempt++ {
			writer, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			_, _ = writer.WriteString("{\"type\":\"user\"}\n")
			_ = writer.Close()
		}
	}()
	_, _, err := readStableFile(path)
	<-done
	if err == nil {
		t.Fatal("readStableFile accepted an unstable read")
	}
	if !strings.Contains(err.Error(), "transcript changed while reading") ||
		!strings.Contains(err.Error(), "the next push picks it up") {
		t.Fatalf("err = %v, want a calm, instructional reason", err)
	}
}
