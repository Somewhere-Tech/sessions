package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	daemonapi "github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestStatusJSONCarriesProfileAndConfigDir(t *testing.T) {
	root := t.TempDir()
	const id = "23000000-0000-4000-8000-000000000001"
	configDir := filepath.Join(root, "profiles", "claude", "work")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "name": "profile status", "description": "", "cmd": "claude", "args": []string{},
				"cwd": root, "profile": "work", "config_dir": configDir, "createdAt": int64(1), "lastDataAt": int64(1), "tool": "claude-code",
			}}})
		case "/api/sessions/" + id + "/verdict":
			response.WriteHeader(http.StatusNotFound)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", root)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "status", id[:8]}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output["profile"] != "work" || output["config_dir"] != configDir {
		t.Fatalf("status profile output=%#v err=%v", output, err)
	}
}

func TestStatusJSONFieldTableAgainstRealScratchSession(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "work")
	for _, directory := range []string{home, repo} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	ledgerPath := filepath.Join(root, "ledger", "lanes.sqlite3")
	t.Setenv("SESSIONS_LEDGER_PATH", ledgerPath)
	gitCommand(t, repo, "init", "-q")
	gitCommand(t, repo, "config", "user.email", "scratch@example.invalid")
	gitCommand(t, repo, "config", "user.name", "Scratch Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repo, "add", "tracked.txt")
	gitCommand(t, repo, "commit", "-q", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: repo, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		GlobalHooksPath: filepath.Join(root, "hooks.json"),
	}
	launcher := prototest.NewLauncher()
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	info, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "status scratch", Description: "Prove the status description contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, ok := manager.Get(info.ID)
	if !ok {
		t.Fatal("created session missing")
	}
	current.SetIdleResult(state.IdleReasonNeedsInput, "Approve the filesystem request?", "Implementation is ready for review.", time.Now().UnixMilli())
	server := httptest.NewServer(daemonapi.New(config, manager, manager.Push()))
	defer server.Close()

	verdictJSON := `{"schemaVersion":1,"verdict":"pass","findings":[{"severity":"info","title":"scratch gate"}],"meta":{"producer":"acceptance"}}`
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "verdict", "emit", info.ID, verdictJSON}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("verdict emit exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	t.Logf("verdict_emit_json=%s", strings.TrimSpace(stdout.String()))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "--json", "status", info.ID[:8]}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout.String())
	}
	t.Logf("status_json=%s", strings.TrimSpace(stdout.String()))
	wantKeys := []string{
		"age_ms", "created_at", "cwd", "description", "description_source", "git", "id",
		"idle_detail", "idle_reason", "idle_since_ms", "kind", "last_activity_at", "last_summary",
		"last_verdict", "name", "runner_protocol", "state", "tool",
	}
	gotKeys := make([]string, 0, len(output))
	for key := range output {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("status keys = %v, want %v\n%s", gotKeys, wantKeys, stdout.String())
	}
	if output["id"] != info.ID || output["name"] != "status scratch" || output["kind"] != "session" || output["state"] != "needs-you" || output["cwd"] != repo {
		t.Fatalf("status identity/state = %#v", output)
	}
	if output["idle_reason"] != state.IdleReasonNeedsInput || output["idle_detail"] != "Approve the filesystem request?" || output["last_summary"] != "Implementation is ready for review." {
		t.Fatalf("status idle summary = %#v", output)
	}
	if output["description"] != "Prove the status description contract" || output["description_source"] != "explicit" {
		t.Fatalf("status description = %#v", output)
	}
	git, ok := output["git"].(map[string]any)
	if !ok || git["dirty_count"] != float64(2) || git["branch"] == "" || git["head"] == "" {
		t.Fatalf("git status = %#v", output["git"])
	}
	last, ok := output["last_verdict"].(map[string]any)
	if !ok || last["verdict"] != "pass" || last["seq"] != float64(1) || last["finding_count"] != float64(1) {
		t.Fatalf("last verdict = %#v", output["last_verdict"])
	}
	for _, key := range []string{"created_at", "last_activity_at"} {
		if _, err := time.Parse(time.RFC3339Nano, output[key].(string)); err != nil {
			t.Fatalf("%s = %q: %v", key, output[key], err)
		}
	}
	t.Logf("scratch session=%s field_table=%v git_branch=%s git_head=%s dirty_count=2 verdict=pass seq=1",
		info.ID, gotKeys, git["branch"], git["head"])
}

func gitCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
