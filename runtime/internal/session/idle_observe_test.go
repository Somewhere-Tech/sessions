package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The Mini's burst was the activity probe under the observe worker, so the
// question this answers is how often that worker runs when nothing happens.
// It is not a timer: activity is recorded from a provider event, and the
// activity tick that does run every few hundred milliseconds writes to the
// ledger only when a session's working state actually changes.
//
// Forty idle sessions, ticking fast, for a second: the ledger must not be
// touched at all.
func TestIdleSessionsDoNotTouchTheLedgerOnTheActivityTick(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: 20 * time.Millisecond,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	ctx := context.Background()
	for index := range 40 {
		if _, err := manager.Create(ctx, state.CreateSessionRequest{
			Cmd: "claude", Cwd: root, Name: fmt.Sprintf("idle-%02d", index),
		}); err != nil {
			t.Fatal(err)
		}
	}
	settled := countLedgerEvents(t, store)
	probes := store.CoalesceProbes()

	// Fifty ticks over forty sessions: two thousand chances to write.
	time.Sleep(time.Second)

	if after := countLedgerEvents(t, store); after != settled {
		t.Fatalf("idle sessions appended %d events over a second of ticks", after-settled)
	}
	if asked := store.CoalesceProbes() - probes; asked != 0 {
		t.Fatalf("idle sessions asked the ledger for a coalescing window %d times", asked)
	}
}

func countLedgerEvents(t *testing.T, store *ledger.Store) int {
	t.Helper()
	events, err := store.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}
