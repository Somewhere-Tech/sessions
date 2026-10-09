package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	ledgerHealthy int32 = iota
	ledgerUnreadable
	ledgerStuck
)

// switchableLedger is the real store until a test breaks it: unreadable
// answers an error, stuck blocks the high-water read until released and
// ignores its context, the way a wedged dependency does.
type switchableLedger struct {
	*ledger.Store
	mode    atomic.Int32
	release chan struct{}
}

func (l *switchableLedger) HighWaterMark(ctx context.Context) (int64, error) {
	switch l.mode.Load() {
	case ledgerUnreadable:
		return 0, errors.New("ledger unreadable")
	case ledgerStuck:
		<-l.release
	}
	return l.Store.HighWaterMark(ctx)
}

func (l *switchableLedger) CurrentStates(ctx context.Context) ([]ledger.LaneState, error) {
	if l.mode.Load() == ledgerUnreadable {
		return nil, errors.New("ledger unreadable")
	}
	return l.Store.CurrentStates(ctx)
}

func newListingDaemon(t *testing.T) (testDaemon, *Server, *switchableLedger, state.SessionInfo) {
	t.Helper()
	daemon := newTestDaemon(t)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	switchable := &switchableLedger{Store: store, release: make(chan struct{})}
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: switchable,
	})
	t.Cleanup(manager.Close)
	handler := New(daemon.config, manager)

	empty := serve(t, handler, http.MethodGet, "/api/sessions?include_exited=1", nil, "127.0.0.1:1", nil)
	var none map[string]any
	decodeBody(t, empty, &none)
	if listed, ok := none["sessions"].([]any); empty.Code != http.StatusOK || !ok || len(listed) != 0 {
		t.Fatalf("empty listing = %d %#v, want 200 with an empty array", empty.Code, none)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root, OperationID: apiStartOperation, PromptOperationID: apiPromptOperation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return daemon, handler, switchable, created
}

// A listing that could not read durable state is a 503 with an instruction,
// not a 200 that has quietly lost recorded sessions and start receipts.
func TestSessionListingRefusesToAnswerWithoutDurableState(t *testing.T) {
	_, handler, switchable, created := newListingDaemon(t)
	healthy := serve(t, handler, http.MethodGet, "/api/sessions?include_exited=1", nil, "127.0.0.1:1", nil)
	if healthy.Code != http.StatusOK || !strings.Contains(healthy.Body.String(), created.ID) || !strings.Contains(healthy.Body.String(), apiStartOperation) {
		t.Fatalf("healthy listing = %d %s", healthy.Code, healthy.Body.String())
	}

	switchable.mode.Store(ledgerUnreadable)
	failed := serve(t, handler, http.MethodGet, "/api/sessions?include_exited=1", nil, "127.0.0.1:1", nil)
	var body map[string]any
	decodeBody(t, failed, &body)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("listing without durable state = %d %#v, want 503", failed.Code, body)
	}
	if _, listed := body["sessions"]; listed || body["code"] != "SESSION_STATE_UNAVAILABLE" || body["action"] != "retry" ||
		!strings.Contains(body["error"].(string), "Nothing was changed") {
		t.Fatalf("503 body = %#v, want an instruction and no sessions", body)
	}
}

// Health counts what the registry holds. It must answer while the ledger is
// stuck, and the count does not depend on durable history.
func TestHealthCountsLoadedSessionsWithoutTheLedger(t *testing.T) {
	_, handler, switchable, _ := newListingDaemon(t)
	switchable.mode.Store(ledgerStuck)
	defer close(switchable.release)

	for _, path := range []string{"/api/health", "/api/health/deep"} {
		answered := make(chan map[string]any, 1)
		go func() {
			// No t.Fatal in here: on failure this goroutine outlives the test
			// until the stuck ledger is released.
			response := serve(t, handler, http.MethodGet, path, nil, "127.0.0.1:1", nil)
			body := map[string]any{"status_code": response.Code}
			if response.Code == http.StatusOK {
				_ = json.Unmarshal(response.Body.Bytes(), &body)
			}
			answered <- body
		}()
		select {
		case body := <-answered:
			if loaded, _ := body["sessionsLoaded"].(float64); loaded != 1 {
				t.Fatalf("%s = %#v, want 200 with sessionsLoaded 1", path, body)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s waited for a stuck ledger", path)
		}
	}
}
