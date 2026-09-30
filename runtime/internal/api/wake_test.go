package api

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A session that stayed paused after a reboot wakes on first contact: the
// message that would have been refused restarts the runner and is delivered.
func TestPausedSessionWakesOnFirstMessage(t *testing.T) {
	daemon := newTestDaemon(t)
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)
	daemon.handler = New(daemon.config, manager)

	id := "paused-after-reboot"
	paths := state.For(daemon.config.RunnerStateDir, id)
	if err := os.MkdirAll(daemon.config.RunnerStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteRunnerMetadata(paths.Meta, state.Metadata{
		ID: id, Name: "night shift", Cmd: "claude", Cwd: daemon.root, Cols: 120, Rows: 40, CreatedAt: time.Now().UnixMilli(), PID: 999999,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteRestorePending(paths.RestorePending, id, "paused after restart; over the automatic limit"); err != nil {
		t.Fatal(err)
	}

	submitted := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+id+"/submit", strings.NewReader(`{"data":"where were we?"}`), "127.0.0.1:1", nil)
	if submitted.Code != http.StatusOK {
		t.Fatalf("submit to paused session status=%d body=%s", submitted.Code, submitted.Body.String())
	}
	if wakes := daemon.launcher.Wakes; len(wakes) != 1 || wakes[0] != id {
		t.Fatalf("launcher wakes = %#v", wakes)
	}
	if _, err := os.Stat(paths.RestorePending); !os.IsNotExist(err) {
		t.Fatalf("paused marker still present: %v", err)
	}
	runner := daemon.launcher.Runner(id)
	if runner == nil || len(runner.Inputs()) == 0 || !strings.Contains(runner.Inputs()[0], "where were we?") {
		t.Fatalf("message did not reach the woken runner: %#v", runner)
	}
	if info, err := manager.WakePaused(context.Background(), id); err != nil || info.ID != id {
		t.Fatalf("second wake of a live session = %#v, %v", info, err)
	}

	// A session that is not paused is not something to wake.
	if _, err := manager.WakePaused(context.Background(), "never-existed"); err == nil {
		t.Fatal("woke a session that does not exist")
	}
}
