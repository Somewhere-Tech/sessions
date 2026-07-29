package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestGoDaemonRunnerMirrorRoundTrip(t *testing.T) {
	// Darwin's Unix socket path limit is short enough that testing.T's nested
	// temp path can exceed it once the UUID filename is appended.
	root, err := os.MkdirTemp("/tmp", "runtime-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runnerBinary := filepath.Join(root, "runner")
	build := exec.Command("go", "build", "-o", runnerBinary, "./cmd/sessions-runner")
	build.Dir = moduleRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go runner: %v\n%s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	config := state.Config{
		Host: "127.0.0.1", Port: port,
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot:       filepath.Join(root, "state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		WebDir:          filepath.Join(root, "web"),
		RunnerPath:      runnerBinary,
	}
	launcher := newProcessLauncher(t, runnerBinary)
	defer launcher.Close()
	registry := state.NewRegistry(config, launcher)
	server := &http.Server{Handler: New(config, registry)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("daemon serve: %v", err)
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	createBody, _ := json.Marshal(state.CreateSessionRequest{
		Cmd: "/bin/bash", Args: []string{"-i"}, Cwd: root,
	})
	createdResponse := doE2ERequest(t, http.MethodPost, baseURL+"/api/sessions", createBody)
	defer createdResponse.Body.Close()
	if createdResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createdResponse.Body)
		t.Fatalf("create status=%d body=%s", createdResponse.StatusCode, body)
	}
	var created state.SessionInfo
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.PID <= 0 {
		t.Fatalf("invalid create response: %#v", created)
	}
	session, ok := registry.Get(created.ID)
	if !ok {
		t.Fatal("created session was not registered")
	}
	attachment := session.Attach(state.AttachOptions{})
	defer attachment.Cancel()

	inputBody, _ := json.Marshal(map[string]string{"data": "echo E2E_$RANDOM\r"})
	inputResponse := doE2ERequest(t, http.MethodPost, baseURL+"/api/sessions/"+created.ID+"/input", inputBody)
	inputResponse.Body.Close()
	if inputResponse.StatusCode != http.StatusOK {
		t.Fatalf("input status=%d", inputResponse.StatusCode)
	}

	markerPattern := regexp.MustCompile(`E2E_[0-9]+`)
	var marker string
	for marker == "" {
		select {
		case event, open := <-attachment.Events:
			if !open {
				t.Fatal("session exited before emitting expanded E2E_$RANDOM marker")
			}
			if match := markerPattern.FindString(event.Output.Data); match != "" {
				marker = match
			}
		case <-t.Context().Done():
			t.Fatal("test ended before expanded E2E_$RANDOM marker arrived")
		}
	}
	snapshotResponse := doE2ERequest(t, http.MethodGet, baseURL+"/api/sessions/"+created.ID+"/snapshot", nil)
	snapshot, readErr := io.ReadAll(snapshotResponse.Body)
	snapshotResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if match := markerPattern.Find(snapshot); match == nil {
		t.Fatalf("snapshot did not retain expanded marker %q", marker)
	}

	killResponse := doE2ERequest(t, http.MethodDelete, baseURL+"/api/sessions/"+created.ID, nil)
	killResponse.Body.Close()
	if killResponse.StatusCode != http.StatusOK {
		t.Fatalf("kill status=%d", killResponse.StatusCode)
	}
	for {
		event, open := <-attachment.Events
		if !open {
			break
		}
		if event.Kind == proto.EventExit || event.Kind == proto.EventRunnerLost {
			break
		}
	}
	exited, _ := session.TerminalState()
	if !exited {
		t.Fatal("session did not retain terminal state")
	}
	t.Logf("round trip session=%s marker=%s snapshot=mirror kill=ok", created.ID, marker)
}

func doE2ERequest(t *testing.T, method, target string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type processLauncher struct {
	runnerPath string

	mu        sync.Mutex
	processes []*testProcess
}

type testProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	waitErr error
}

func newProcessLauncher(t *testing.T, runnerPath string) *processLauncher {
	t.Helper()
	return &processLauncher{runnerPath: runnerPath}
}

func (l *processLauncher) ProgramArguments(proto.LaunchRequest) []string {
	return []string{l.runnerPath}
}

func (l *processLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	command := exec.Command(l.runnerPath)
	command.Dir = request.Info.Cwd
	keys := make([]string, 0, len(request.Env))
	for key := range request.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	command.Env = make([]string, 0, len(keys))
	for _, key := range keys {
		command.Env = append(command.Env, key+"="+request.Env[key])
	}
	logPath := filepath.Join(filepath.Dir(request.Info.SocketPath), request.Info.ID+"-process.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	process := &testProcess{command: command, done: make(chan struct{})}
	l.mu.Lock()
	l.processes = append(l.processes, process)
	l.mu.Unlock()
	go func() {
		process.mu.Lock()
		process.waitErr = command.Wait()
		process.mu.Unlock()
		_ = logFile.Close()
		close(process.done)
	}()

	var lastErr error
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		runner, err := l.Attach(ctx, request.Info)
		if err == nil {
			return runner, nil
		}
		lastErr = err
		select {
		case <-process.done:
			process.mu.Lock()
			processErr := process.waitErr
			process.mu.Unlock()
			return nil, fmt.Errorf("runner exited before attach: %v (last dial: %v; log: %s)", processErr, lastErr, logPath)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *processLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	return proto.DialRunner(ctx, info.SocketPath)
}

func (l *processLauncher) Close() {
	l.mu.Lock()
	processes := append([]*testProcess(nil), l.processes...)
	l.mu.Unlock()
	for _, process := range processes {
		if process.command.Process == nil {
			continue
		}
		_ = process.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(2 * time.Second):
			_ = process.command.Process.Kill()
			<-process.done
		}
	}
}
