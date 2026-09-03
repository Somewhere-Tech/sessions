package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestDeleteClosesLedgerOnlyLostLane(t *testing.T) {
	root := t.TempDir()
	config := state.Config{
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		ProcessAlive: func(int) bool { return false }, ProcessCommand: func(int) string { return "" },
	})
	t.Cleanup(manager.Close)

	const id = "44000000-0000-4000-8000-000000000001"
	if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
		Meta: ledger.Meta{LaneID: id, AtMS: 100}, LaneUUID: id, Name: "sleeper",
		Tool: string(state.ToolLane), Cwd: root, CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerLost(context.Background(), ledger.Observation{
		Meta: ledger.Meta{LaneID: id, AtMS: 200},
	}); err != nil {
		t.Fatal(err)
	}

	server := New(config, manager, manager.Push())
	before := serve(t, server, http.MethodGet, "/api/lanes", nil, "127.0.0.1:1", nil)
	for _, want := range []string{`"runnerGone":true`, `"state":"lost"`, `"command":"sessions kill ` + id + `"`} {
		if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), want) {
			t.Fatalf("lost lane listing omitted %s: status=%d body=%s", want, before.Code, before.Body.String())
		}
	}
	response := serve(t, server, http.MethodDelete, "/api/sessions/"+id, strings.NewReader(`{}`), "127.0.0.1:1", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("close lost lane status=%d body=%s", response.Code, response.Body.String())
	}
	listed := manager.List(true)
	if len(listed) != 1 || !listed[0].Exited || listed[0].Unreachable {
		t.Fatalf("closed lost lane = %#v", listed)
	}
}
