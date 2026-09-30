package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Reported from the Mini, 11 September: after a restart with 593 sessions the
// daemon spent about three minutes re-attaching runners, and during that window
// `sessions wait <lane>` answered "no live session matches" for a lane that was
// running the whole time. Health reported restore.pending and nothing else, so
// no caller could tell "still loading" from "gone".
func TestHealthSaysWhetherTheDaemonIsStillLoading(t *testing.T) {
	daemon, manager, _ := newSlowStartupDaemon(t, 6, 80*time.Millisecond)
	defer manager.Close()

	loading := make(chan struct{})
	go func() {
		close(loading)
		_ = manager.Discover(context.Background())
	}()
	<-loading

	// While the pass runs, health says which phase the daemon is in and how far
	// through it is — and the daemon answers at all, which is the other half of
	// the promise.
	sawLoading := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		startup := startupFromHealth(t, daemon)
		if startup["phase"] == "loading" {
			sawLoading = true
			if startup["startedAt"].(float64) <= 0 {
				t.Fatalf("startup.startedAt = %#v, want when this process began loading", startup["startedAt"])
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawLoading {
		t.Fatal("health never reported the loading phase during a deliberately slow startup")
	}

	for time.Now().Before(deadline) {
		startup := startupFromHealth(t, daemon)
		if startup["phase"] == "ready" {
			if loaded := startup["loaded"].(float64); loaded < 1 {
				t.Fatalf("a finished startup loaded %v of %v sessions: %#v", loaded, startup["total"], startup)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon never reached the ready phase")
}

// A daemon that cannot report the phase reads as ready, which is what every
// caller assumed before the field existed.
func TestHealthReportsReadyWhenTheRuntimeCannotSay(t *testing.T) {
	daemon := newTestDaemon(t)
	startup := startupFromHealth(t, daemon.handler)
	if startup["phase"] != "ready" {
		t.Fatalf("startup = %#v, want a runtime that cannot answer to read as ready", startup)
	}
}

// Creating a session while the daemon re-attaches is safe: create takes no lock
// the discovery pass holds. What made it look like a hang was silence, not a
// deadlock, so this pins that it still answers.
func TestCreatingASessionDuringStartupStillAnswers(t *testing.T) {
	daemon, manager, root := newSlowStartupDaemon(t, 6, 80*time.Millisecond)
	defer manager.Close()
	go func() { _ = manager.Discover(context.Background()) }()

	body, _ := json.Marshal(state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	started := time.Now()
	response := serve(t, daemon, http.MethodPost, "/api/sessions", bytes.NewReader(body), "127.0.0.1:4321",
		http.Header{"Content-Type": {"application/json"}})
	if response.Code != http.StatusCreated {
		t.Fatalf("create during startup: status=%d body=%s", response.Code, response.Body.String())
	}
	// Not a hang: a person waiting on this must get an answer in seconds, not
	// after the whole re-attach.
	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("create during startup took %s", took)
	}
}

func startupFromHealth(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	response := serve(t, handler, http.MethodGet, "/api/health", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
	var body struct {
		Startup map[string]any `json:"startup"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Startup == nil {
		t.Fatalf("health carries no startup block: %s", response.Body.String())
	}
	return body.Startup
}

// newSlowStartupDaemon is a daemon with runner artifacts on disk whose attach
// is deliberately slow, which is the only way to observe what it says while it
// is still loading.
func newSlowStartupDaemon(t *testing.T, runners int, attachDelay time.Duration) (*Server, *sessionruntime.Manager, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
		TokenPath: filepath.Join(root, "state", "token"), WebDir: filepath.Join(root, "web"),
	}
	if err := state.EnsureDir(config.RunnerStateDir); err != nil {
		t.Fatal(err)
	}
	launcher := prototest.NewLauncher()
	launcher.AttachDelay = attachDelay
	for index := range runners {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		paths := state.For(config.RunnerStateDir, id)
		if err := state.WriteMetadata(paths.Meta, state.Metadata{
			ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 120, Rows: 40,
			CreatedAt: time.Now().UnixMilli(), PID: 4242, SockPath: paths.Socket,
		}); err != nil {
			t.Fatal(err)
		}
		// Discovery keys off the socket artifact on Unix. The file's presence is
		// what a restarted daemon finds; the fake launcher answers for it.
		if err := os.WriteFile(paths.Socket, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		launcher.Runners[id] = prototest.NewRunner(proto.RunnerInfo{
			ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 120, Rows: 40,
			PID: 4242, SocketPath: paths.Socket, ProtocolVersion: proto.ProtocolVersion,
		})
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		LedgerReader: store, Retention: store.Retention(),
	})
	return New(config, manager), manager, root
}
