package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestCreateHeadersBecomeValidatedLedgerProvenance(t *testing.T) {
	root := t.TempDir()
	config := state.Config{
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	server := New(config, manager, manager.Push())
	body, _ := json.Marshal(state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})

	parentResponse := serve(t, server, http.MethodPost, "/api/sessions", bytes.NewReader(body), "127.0.0.1:1", nil)
	if parentResponse.Code != http.StatusCreated {
		t.Fatalf("parent status=%d body=%s", parentResponse.Code, parentResponse.Body.String())
	}
	var parent state.SessionInfo
	decodeBody(t, parentResponse, &parent)

	childResponse := serve(t, server, http.MethodPost, "/api/lanes", bytes.NewReader(body), "127.0.0.1:1", http.Header{
		creatorSessionHeader: {parent.ID},
	})
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("child status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	var child state.SessionInfo
	decodeBody(t, childResponse, &child)
	states := ledger.Fold(mustEvents(t, store, child.ID))
	if len(states) != 1 || states[0].CreatorKind != ledger.CreatorSession || states[0].CreatorID != parent.ID {
		t.Fatalf("child ledger provenance = %#v", states)
	}

	secondParentResponse := serve(t, server, http.MethodPost, "/api/sessions", bytes.NewReader(body), "127.0.0.1:1", nil)
	if secondParentResponse.Code != http.StatusCreated {
		t.Fatalf("second parent status=%d body=%s", secondParentResponse.Code, secondParentResponse.Body.String())
	}
	var secondParent state.SessionInfo
	decodeBody(t, secondParentResponse, &secondParent)
	grouped := serve(
		t,
		server,
		http.MethodPut,
		"/api/sessions/"+child.ID+"/display-parent",
		strings.NewReader(`{"parentSessionId":"`+secondParent.ID+`"}`),
		"127.0.0.1:1",
		nil,
	)
	if grouped.Code != http.StatusOK {
		t.Fatalf("group child status=%d body=%s", grouped.Code, grouped.Body.String())
	}
	var groupedChild state.SessionInfo
	for _, candidate := range manager.List(true) {
		if candidate.ID == child.ID {
			groupedChild = candidate
			break
		}
	}
	if groupedChild.ParentSessionID != parent.ID {
		t.Fatalf("display grouping rewrote creator parent: got %q want %q", groupedChild.ParentSessionID, parent.ID)
	}
	if groupedChild.DisplayParentSessionID == nil || *groupedChild.DisplayParentSessionID != secondParent.ID {
		t.Fatalf("display parent = %#v, want %q", groupedChild.DisplayParentSessionID, secondParent.ID)
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, child.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DisplayParentSessionID == nil || *metadata.DisplayParentSessionID != secondParent.ID {
		t.Fatalf("persisted display parent = %#v, want %q", metadata.DisplayParentSessionID, secondParent.ID)
	}
	cycle := serve(
		t,
		server,
		http.MethodPut,
		"/api/sessions/"+secondParent.ID+"/display-parent",
		strings.NewReader(`{"parentSessionId":"`+child.ID+`"}`),
		"127.0.0.1:1",
		nil,
	)
	if cycle.Code != http.StatusBadRequest || !strings.Contains(cycle.Body.String(), "descendants") {
		t.Fatalf("cycle status=%d body=%s", cycle.Code, cycle.Body.String())
	}

	setAside := serve(
		t,
		server,
		http.MethodPut,
		"/api/sessions/"+child.ID+"/set-aside",
		strings.NewReader(`{"setAside":true}`),
		"127.0.0.1:1",
		nil,
	)
	if setAside.Code != http.StatusOK || !strings.Contains(setAside.Body.String(), `"setAsideAt":`) {
		t.Fatalf("set aside status=%d body=%s", setAside.Code, setAside.Body.String())
	}
	var setAsideChild state.SessionInfo
	for _, candidate := range manager.List(true) {
		if candidate.ID == child.ID {
			setAsideChild = candidate
			break
		}
	}
	if setAsideChild.SetAsideAt == nil {
		t.Fatal("set-aside timestamp was not exposed through SessionInfo")
	}
	metadata, err = state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, child.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SetAsideAt == nil {
		t.Fatal("set-aside timestamp was not persisted in runner metadata")
	}
	input := serve(
		t,
		server,
		http.MethodPost,
		"/api/sessions/"+child.ID+"/input",
		strings.NewReader(`{"data":"bring this back"}`),
		"127.0.0.1:1",
		nil,
	)
	if input.Code != http.StatusOK {
		t.Fatalf("input status=%d body=%s", input.Code, input.Body.String())
	}
	current, ok := manager.Get(child.ID)
	if !ok || current.Info().SetAsideAt != nil {
		t.Fatal("user-directed input did not bring the session back to the working set")
	}
	metadata, err = state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, child.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SetAsideAt != nil {
		t.Fatal("input did not clear persisted set-aside metadata")
	}
	setAsideAgain := serve(
		t,
		server,
		http.MethodPut,
		"/api/sessions/"+child.ID+"/set-aside",
		strings.NewReader(`{"setAside":true}`),
		"127.0.0.1:1",
		nil,
	)
	if setAsideAgain.Code != http.StatusOK {
		t.Fatalf("set aside before exit status=%d body=%s", setAsideAgain.Code, setAsideAgain.Body.String())
	}
	if err := manager.RequestKill(context.Background(), child.ID, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current, present := manager.Get(child.ID)
		if present && current.Info().Exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test session did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, ok = manager.Get(child.ID)
	if !ok || current.Info().SetAsideAt == nil {
		t.Fatal("ended in-memory record did not retain the prior set-aside fact")
	}
	endedAside := serve(
		t,
		server,
		http.MethodPut,
		"/api/sessions/"+child.ID+"/set-aside",
		strings.NewReader(`{"setAside":true}`),
		"127.0.0.1:1",
		nil,
	)
	if endedAside.Code != http.StatusConflict || !strings.Contains(endedAside.Body.String(), "use archive") {
		t.Fatalf("ended set aside status=%d body=%s", endedAside.Code, endedAside.Body.String())
	}

	staleResponse := serve(t, server, http.MethodPost, "/api/lanes", bytes.NewReader(body), "127.0.0.1:1", http.Header{
		creatorSessionHeader: {"00000000-0000-4000-8000-000000000099"},
	})
	if staleResponse.Code != http.StatusBadRequest || !strings.Contains(staleResponse.Body.String(), "has no created event") {
		t.Fatalf("stale parent status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	forged := serve(t, server, http.MethodPost, "/api/lanes", bytes.NewReader(body), "127.0.0.1:1", http.Header{
		creatorSessionHeader: {"forged-parent"},
	})
	if forged.Code != http.StatusBadRequest || !strings.Contains(forged.Body.String(), "invalid creator session UUID") {
		t.Fatalf("forged parent status=%d body=%s", forged.Code, forged.Body.String())
	}
	missing := serve(t, server, http.MethodPost, "/api/lanes", bytes.NewReader(body), "127.0.0.1:1", http.Header{
		creatorSessionHeader: {""},
	})
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "non-empty") {
		t.Fatalf("missing parent status=%d body=%s", missing.Code, missing.Body.String())
	}
	combined := serve(t, server, http.MethodPost, "/api/lanes", bytes.NewReader(body), "127.0.0.1:1", http.Header{
		creatorSessionHeader: {parent.ID}, creatorOwnerHeader: {"external"},
	})
	if combined.Code != http.StatusBadRequest || !strings.Contains(combined.Body.String(), "cannot be combined") {
		t.Fatalf("combined headers status=%d body=%s", combined.Code, combined.Body.String())
	}
}

func mustEvents(t *testing.T, store *ledger.Store, id string) []ledger.Event {
	t.Helper()
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestHeadlessLaneLifecycleManifestAndLedger(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "sessions-lane-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "init", "--quiet", repo)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	specPath := filepath.Join(repo, "lane-spec.md")
	if err := os.WriteFile(specPath, []byte("scratch lane\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runnerBinary := filepath.Join(base, "runner")
	build := exec.Command("go", "build", "-o", runnerBinary, "./cmd/sessions-runner")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, output)
	}
	config := state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: repo, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(base, "state"), UserStateRoot: filepath.Join(base, "user-state"),
		RunnerStateDir:  filepath.Join(base, "state", "runners"),
		LaunchAgentsDir: filepath.Join(base, "agents"), RunnerPath: runnerBinary,
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(base, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher := newProcessLauncher(t, runnerBinary)
	defer launcher.Close()
	notifications := make(chan sessionruntime.PushPayload, 1)
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(payload sessionruntime.PushPayload) { notifications <- payload },
	})
	defer manager.Close()
	server := httptest.NewServer(New(config, manager, manager.Push()))
	defer server.Close()

	requestBody, _ := json.Marshal(state.CreateSessionRequest{
		Cmd: "/bin/sh", Args: []string{"-c", "sleep 0.1; printf 'lane edit\\n' > created-by-lane.txt; printf 'OUT_MARKER\\n'; printf 'ERR_MARKER\\n' >&2; exit 3"},
		Cwd: repo, Name: "scratch failure", SpecPath: specPath,
	})
	createdResponse := doE2ERequest(t, http.MethodPost, server.URL+"/api/lanes", requestBody)
	defer createdResponse.Body.Close()
	if createdResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createdResponse.Body)
		t.Fatalf("create lane status=%d body=%s", createdResponse.StatusCode, body)
	}
	var created state.SessionInfo
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != state.KindLane || created.Tool != "lane:sh" || created.SpecPath != specPath {
		t.Fatalf("lane create response = %#v", created)
	}

	notification := <-notifications
	if notification.Title != "🔴 scratch failure died (exit 3)" || notification.Body != "ERR_MARKER" {
		t.Fatalf("lane notification = %#v", notification)
	}
	session, ok := manager.Get(created.ID)
	if !ok || !session.Info().Exited {
		t.Fatalf("lane did not retain exited state: ok=%v", ok)
	}
	manifest, err := state.ReadCompletionManifest(state.For(config.RunnerStateDir, created.ID).Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExitCode != 3 || manifest.DurationMS < 50 || manifest.SpecPath != specPath ||
		!strings.Contains(manifest.LastOutputTail, "OUT_MARKER") || !strings.Contains(manifest.LastOutputTail, "ERR_MARKER") ||
		manifest.FilesChanged == nil || *manifest.FilesChanged < 1 || len([]byte(manifest.LastOutputTail)) > 4*1024 {
		t.Fatalf("completion manifest = %#v", manifest)
	}

	snapshotResponse := doE2ERequest(t, http.MethodGet, server.URL+"/api/sessions/"+created.ID+"/snapshot", nil)
	snapshot, _ := io.ReadAll(snapshotResponse.Body)
	snapshotResponse.Body.Close()
	if !bytes.Contains(snapshot, []byte("OUT_MARKER")) || !bytes.Contains(snapshot, []byte("ERR_MARKER")) {
		t.Fatalf("lane snapshot = %q", snapshot)
	}
	manifestResponse := doE2ERequest(t, http.MethodGet, server.URL+"/api/lanes/"+created.ID+"/manifest", nil)
	manifestResponse.Body.Close()
	if manifestResponse.StatusCode != http.StatusOK {
		t.Fatalf("manifest endpoint status=%d", manifestResponse.StatusCode)
	}

	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []ledger.EventType{ledger.EventCreated, ledger.EventLaunchStarted, ledger.EventRunnerExited}
	for _, want := range wantTypes {
		found := false
		for _, event := range events {
			if event.Type == want {
				found = true
				if want == ledger.EventRunnerExited && !strings.Contains(string(event.Payload), `"code":3`) {
					t.Fatalf("runner_exited payload = %s", event.Payload)
				}
			}
		}
		if !found {
			t.Fatalf("ledger events %#v missing %s", events, want)
		}
	}
}
