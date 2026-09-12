package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/background"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Reported from the MacBook, 11 September: across four restarts with the same
// persisted cache, the first listing's store stage cost between 0.3 s and 4.0 s.
// The warm and the first request do the same work, and when they do it at the
// same time the person waiting is the one who pays. The warm exists so nobody
// waits for it — so when somebody is already waiting, it stands aside.
func TestTheStartupWarmStandsAsideForSomebodyWhoAsked(t *testing.T) {
	daemon, manager := newBusyHistoryDaemon(t, 600)
	defer manager.Close()
	before := background.Totals()["history-warm"]

	daemon.WarmHistory(nil)
	// A client that was already open asks immediately, the way the app does.
	response := serve(t, daemon, http.MethodGet, "/api/history?timing=1", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var body struct {
		Sessions []json.RawMessage `json:"sessions"`
		Timing   map[string]any    `json:"timing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) < 600 {
		t.Fatalf("the fixture listed %d conversations, want 600", len(body.Sessions))
	}
	// That listing did the work itself, which is exactly what the warm would
	// otherwise have been doing beside it.
	t.Logf("the first listing took %vms in the store", body.Timing["store_ms"])

	time.Sleep(historyWarmGrace + 500*time.Millisecond)
	if runs := background.Totals()["history-warm"].Runs - before.Runs; runs != 0 {
		t.Fatalf("the warm ran %d times beside the request it exists to spare", runs)
	}
}

// And when nobody asks, it still does the work — which is the whole point of
// having it. It says what it cost, under its own name.
func TestTheWarmRunsAndNamesItselfWhenNobodyAsks(t *testing.T) {
	daemon, manager := newBusyHistoryDaemon(t, 600)
	defer manager.Close()
	before := background.Totals()["history-warm"]

	daemon.WarmHistory(nil)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if background.Totals()["history-warm"].Runs > before.Runs {
			after := background.Totals()["history-warm"]
			t.Logf("the warm over 600 conversations took %s", after.Wall-before.Wall)
			if after.Wall <= before.Wall {
				t.Fatal("the warm was counted without any time")
			}
			// A listing afterwards is served from what the warm computed.
			listing := timeListing(t, daemon)
			if read, ok := listing["store_cards_read"].(float64); ok && read > 0 {
				t.Fatalf("a listing after the warm still read %v cards", read)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the warm never ran, and nobody had asked for a listing")
}

// newBusyHistoryDaemon is a daemon whose provider history holds `count`
// conversations: enough that the work the warm does is measurable, and all of
// it inside this test's own HOME.
func newBusyHistoryDaemon(t *testing.T, count int) (*Server, *sessionruntime.Manager) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	writeFixtureRollouts(t, filepath.Join(root, ".codex", "sessions", "2026", "09", "11"), count)
	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
		TokenPath: filepath.Join(root, "state", "token"), WebDir: filepath.Join(root, "web"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		LedgerReader: store, Retention: store.Retention(),
	})
	return New(config, manager), manager
}

func writeFixtureRollouts(t *testing.T, dir string, count int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Dir(dir)
	for index := range count {
		id := fmt.Sprintf("%08x-cccc-4ccc-8ccc-cccccccccccc", index)
		lines := fmt.Sprintf(
			`{"timestamp":"2026-09-11T00:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":"2026-09-11T00:00:00Z","originator":"codex_cli_rs"}}`+"\n"+
				`{"timestamp":"2026-09-11T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"conversation %d"}]}}`+"\n",
			id, cwd, index)
		if err := os.WriteFile(filepath.Join(dir, "rollout-"+id+".jsonl"), []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
