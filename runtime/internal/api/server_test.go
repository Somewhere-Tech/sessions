package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type testDaemon struct {
	config   state.Config
	registry *state.Registry
	launcher *prototest.Launcher
	handler  *Server
	root     string
}

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
	for _, key := range []string{"ok", "name", "version", "listen", "lan", "access", "system", "compatibility", "discovering", "sessionsLoaded"} {
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
	if runnerCompatibility["minimum"] != float64(0) || runnerCompatibility["maximum"] != float64(2) {
		t.Fatalf("unexpected runner compatibility: %#v", runnerCompatibility)
	}

	deep := serve(t, daemon.handler, http.MethodGet, "/api/health/deep", nil, "198.51.100.10:4321", nil)
	decodeBody(t, deep, &body)
	for _, key := range []string{"uptimeSec", "sessions"} {
		if _, exists := body[key]; !exists {
			t.Errorf("deep health missing key %q: %#v", key, body)
		}
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

func serve(t *testing.T, handler http.Handler, method, target string, body io.Reader, remote string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
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
		case <-t.Context().Done():
			t.Fatal("test ended before fake runner state changed")
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
