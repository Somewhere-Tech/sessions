package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Run explicitly with:
//
//	CXWIRE_INTEGRATION=1 CGO_ENABLED=0 go test -v ./cmd/sessions -run '^TestCXWireRealTurnRestartReattach$' -count=1
func TestCXWireRealTurnRestartReattach(t *testing.T) {
	if os.Getenv("CXWIRE_INTEGRATION") != "1" {
		t.Skip("set CXWIRE_INTEGRATION=1 to spend a real Codex turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	root, err := os.MkdirTemp("/tmp", "sessions-cxwire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	originalHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	authSource := filepath.Join(originalHome, ".codex", "auth.json")
	if _, err := os.Stat(authSource); err != nil {
		t.Fatalf("Codex authentication is required at %s: %v", authSource, err)
	}
	if err := os.Symlink(authSource, filepath.Join(codexHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("SESSIONS_CODEX_APPSERVER", "1")

	appSocket := filepath.Join(root, "app-server.sock")
	appLog, err := os.OpenFile(filepath.Join(root, "app-server.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer appLog.Close()
	appServer := exec.CommandContext(ctx, codexPath, "app-server", "--listen", "unix://"+appSocket)
	appServer.Env = os.Environ()
	appServer.Stdout = appLog
	appServer.Stderr = appLog
	if err := appServer.Start(); err != nil {
		t.Fatal(err)
	}
	appDone := make(chan error, 1)
	go func() { appDone <- appServer.Wait() }()
	t.Cleanup(func() {
		if appServer.Process != nil {
			_ = appServer.Process.Kill()
		}
		select {
		case <-appDone:
		case <-time.After(2 * time.Second):
		}
	})
	awaitPath(t, ctx, appSocket, appDone, appLog)
	t.Setenv("SESSIONS_CODEX_APP_SERVER_SOCKET", appSocket)

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

	createdOutput := runCXWireCLI(t, server.URL, "--json", "new", "--tool", "codex", "--cwd", work)
	var created session
	if err := json.Unmarshal(createdOutput, &created); err != nil {
		t.Fatalf("decode sessions new: %v\n%s", err, createdOutput)
	}
	if created.Kind != state.KindCodexAppServer || created.ConversationID == "" {
		t.Fatalf("sessions new metadata = %#v", created)
	}
	conversationID := created.ConversationID
	t.Logf("SESSION_ID %s", created.ID)
	t.Logf("CONVERSATION_ID %s", conversationID)
	t.Logf("REMOTE_ENDPOINT %s", created.RemoteEndpoint)
	t.Logf("METADATA kind=%s conversationId=%s", created.Kind, created.ConversationID)

	type cliResult struct {
		output []byte
		stderr string
		code   int
	}
	sendDone := make(chan cliResult, 1)
	go func() {
		arguments := []string{"--host", server.URL, "--json", "send", created.ID, "--timeout", "60s", "Reply with exactly CXWIRE_OK."}
		var stdout, stderr bytes.Buffer
		code := run(arguments, strings.NewReader(""), &stdout, &stderr)
		sendDone <- cliResult{output: append([]byte(nil), stdout.Bytes()...), stderr: stderr.String(), code: code}
	}()

	sawWorking := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(created.ID)
		if !ok {
			t.Fatal("structured session disappeared during turn")
		}
		info := current.Info()
		if info.Working {
			sawWorking = true
		}
		if sawWorking && !info.Working && historyContainsAssistant(current.EventsWindow(nil, nil, nil).Events, "CXWIRE_OK") {
			break
		}
		select {
		case result := <-sendDone:
			if result.code != 0 {
				t.Fatalf("sessions send: exit=%d stderr=%q stdout=%q", result.code, result.stderr, result.output)
			}
			t.Logf("SEND %s", strings.TrimSpace(string(result.output)))
			sendDone = nil
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sendDone != nil {
		select {
		case result := <-sendDone:
			if result.code != 0 {
				t.Fatalf("sessions send: exit=%d stderr=%q stdout=%q", result.code, result.stderr, result.output)
			}
			t.Logf("SEND %s", strings.TrimSpace(string(result.output)))
		case <-time.After(5 * time.Second):
			t.Fatal("sessions send did not return after the structured user event")
		}
	}
	current, ok := manager.Get(created.ID)
	if !ok || current.Info().Working || !sawWorking {
		t.Fatalf("working lifecycle: sawWorking=%v current=%#v", sawWorking, current)
	}
	t.Log("WORKING_STATE working -> idle")
	if !historyContainsAssistant(current.EventsWindow(nil, nil, nil).Events, "CXWIRE_OK") {
		t.Fatalf("structured history did not contain CXWIRE_OK: %s", current.EventsWindow(nil, nil, nil).Events)
	}
	last := runCXWireCLI(t, server.URL, "--json", "last", created.ID, "--role", "assistant")
	assertCXWireLast(t, last, "CXWIRE_OK")
	t.Logf("LAST_BEFORE_RESTART %s", strings.TrimSpace(string(last)))

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
		t.Fatal("daemon restart did not rediscover structured session")
	}
	reattachedInfo := reattached.Info()
	if reattachedInfo.ConversationID != conversationID {
		t.Fatalf("reattached conversation = %q, want %q", reattachedInfo.ConversationID, conversationID)
	}
	if !historyContainsAssistant(reattached.EventsWindow(nil, nil, nil).Events, "CXWIRE_OK") {
		t.Fatalf("reattached structured history = %s", reattached.EventsWindow(nil, nil, nil).Events)
	}
	restartedServer := httptest.NewServer(api.New(config, restarted, restarted.Push()))
	defer restartedServer.Close()
	last = runCXWireCLI(t, restartedServer.URL, "--json", "last", created.ID, "--role", "assistant")
	assertCXWireLast(t, last, "CXWIRE_OK")
	t.Logf("LAST_AFTER_RESTART %s", strings.TrimSpace(string(last)))

	remoteSocket := strings.TrimPrefix(reattachedInfo.RemoteEndpoint, "unix://")
	if remoteSocket == "" {
		t.Fatalf("remote endpoint is not concrete: %q", reattachedInfo.RemoteEndpoint)
	}
	if info, err := os.Stat(remoteSocket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("remote endpoint is not a reachable Unix socket: %q: %v", remoteSocket, err)
	}
	t.Logf("REATTACHED conversation=%s history=%d remote=%s", conversationID, reattached.ClaudeEventCount(), reattachedInfo.RemoteEndpoint)
	if err := restarted.RequestKill(ctx, created.ID, true); err != nil {
		t.Fatal(err)
	}
}

type cxwireProcessLauncher struct {
	runnerBinary string
	logPath      string
	mu           sync.Mutex
	processes    []*exec.Cmd
}

func newCXWireProcessLauncher(t *testing.T, runnerBinary, logPath string) *cxwireProcessLauncher {
	launcher := &cxwireProcessLauncher{runnerBinary: runnerBinary, logPath: logPath}
	t.Cleanup(func() {
		launcher.mu.Lock()
		processes := append([]*exec.Cmd(nil), launcher.processes...)
		launcher.mu.Unlock()
		for _, command := range processes {
			if command.Process != nil {
				_ = command.Process.Kill()
			}
		}
	})
	return launcher
}

func (l *cxwireProcessLauncher) ProgramArguments(proto.LaunchRequest) []string {
	return []string{l.runnerBinary}
}

func (l *cxwireProcessLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	logFile, err := os.OpenFile(l.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	command := exec.Command(l.runnerBinary)
	command.Env = mergedEnvironment(request.Env)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	l.mu.Lock()
	l.processes = append(l.processes, command)
	l.mu.Unlock()
	go func() {
		_ = command.Wait()
		_ = logFile.Close()
	}()
	return l.waitAndAttach(ctx, request.Info)
}

func (l *cxwireProcessLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	return proto.DialRunner(ctx, info.SocketPath)
}

func (l *cxwireProcessLauncher) Reap(_ string) error {
	return nil
}

func (l *cxwireProcessLauncher) waitAndAttach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		runner, err := l.Attach(ctx, info)
		if err == nil {
			return runner, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	encoded, _ := os.ReadFile(l.logPath)
	return nil, fmt.Errorf("runner did not attach: %w\n%s", lastErr, encoded)
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func runCXWireCLI(t *testing.T, serverURL string, args ...string) []byte {
	t.Helper()
	arguments := append([]string{"--host", serverURL}, args...)
	var stdout, stderr bytes.Buffer
	if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("sessions %s: exit=%d stderr=%q stdout=%q", strings.Join(args, " "), code, stderr.String(), stdout.String())
	}
	return append([]byte(nil), stdout.Bytes()...)
}

func assertCXWireLast(t *testing.T, encoded []byte, want string) {
	t.Helper()
	var turns []messageTurn
	if err := json.Unmarshal(encoded, &turns); err != nil {
		t.Fatalf("decode sessions last: %v\n%s", err, encoded)
	}
	if len(turns) != 1 || strings.TrimSpace(turns[0].Text) != want {
		t.Fatalf("sessions last = %#v, want assistant %q", turns, want)
	}
}

func historyContainsAssistant(events []json.RawMessage, text string) bool {
	for _, raw := range events {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil || event["type"] != "assistant" {
			continue
		}
		if event["source"] != "codex-app-server" {
			continue
		}
		message, _ := event["message"].(map[string]any)
		if codexMessageTextForTest(message["content"]) == text {
			return true
		}
	}
	return false
}

func codexMessageTextForTest(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, _ := content.([]any)
	var output strings.Builder
	for _, value := range blocks {
		block, _ := value.(map[string]any)
		text, _ := block["text"].(string)
		output.WriteString(text)
	}
	return output.String()
}

func awaitPath(t *testing.T, ctx context.Context, path string, process <-chan error, logFile *os.File) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-process:
			_, _ = logFile.Seek(0, io.SeekStart)
			encoded, _ := io.ReadAll(logFile)
			t.Fatalf("app-server exited before socket: %v\n%s", err, encoded)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}
