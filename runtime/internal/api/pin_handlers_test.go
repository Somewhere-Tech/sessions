package api

import (
	"bytes"
	"context"
	"encoding/json"
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

// The pin route is the daemon side of the user marking a workbench. It has to
// persist the mark, report it back on the session record, and say the same
// thing to the next reader of the metadata file, because that file is what a
// restarted daemon rebuilds from.
func TestPinRoutePersistsAndExposesTheMark(t *testing.T) {
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

	created := serve(t, server, http.MethodPost, "/api/sessions", bytes.NewReader(body), "127.0.0.1:1", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var session state.SessionInfo
	decodeBody(t, created, &session)
	if session.Pinned {
		t.Fatal("a new session came back pinned")
	}

	pin := serve(t, server, http.MethodPut, "/api/sessions/"+session.ID+"/pin",
		strings.NewReader(`{"pinned":true}`), "127.0.0.1:1", nil)
	if pin.Code != http.StatusOK || !strings.Contains(pin.Body.String(), `"pinned":true`) {
		t.Fatalf("pin status=%d body=%s", pin.Code, pin.Body.String())
	}
	if !listedSession(t, manager, session.ID).Pinned {
		t.Fatal("the pin was accepted but the session record does not report it, so " +
			"nothing reading the listing can sort or protect the session")
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, session.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.Pinned {
		t.Fatal("the pin was accepted but never reached runner metadata, so it is gone " +
			"at the next daemon restart")
	}

	unpin := serve(t, server, http.MethodPut, "/api/sessions/"+session.ID+"/pin",
		strings.NewReader(`{"pinned":false}`), "127.0.0.1:1", nil)
	if unpin.Code != http.StatusOK || !strings.Contains(unpin.Body.String(), `"pinned":false`) {
		t.Fatalf("unpin status=%d body=%s", unpin.Code, unpin.Body.String())
	}
	if listedSession(t, manager, session.ID).Pinned {
		t.Fatal("unpinning left the session marked")
	}

	// An ended record cannot be exempted from a termination that already
	// happened, so the pin is refused with the verb that does work on it.
	if err := manager.RequestKill(context.Background(), session.ID, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, present := manager.Get(session.ID)
		if present && current.Info().Exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test session did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ended := serve(t, server, http.MethodPut, "/api/sessions/"+session.ID+"/pin",
		strings.NewReader(`{"pinned":true}`), "127.0.0.1:1", nil)
	if ended.Code != http.StatusConflict || !strings.Contains(ended.Body.String(), "archive") {
		t.Fatalf("ended pin status=%d body=%s, want 409 pointing at archive", ended.Code, ended.Body.String())
	}
}

func listedSession(t *testing.T, manager *sessionruntime.Manager, id string) state.SessionInfo {
	t.Helper()
	for _, candidate := range manager.List(true) {
		if candidate.ID == id {
			return candidate
		}
	}
	t.Fatalf("session %s is not listed", id)
	return state.SessionInfo{}
}

// A wrong verb has to answer 405 the way every other route family does. The
// session router's catch-all is 404, which would tell a caller its session no
// longer exists when only its method was wrong -- and for a pin, "gone" is the
// one answer that would make a caller stop looking for a session it still has.
func TestPinRouteRefusesTheWrongMethodWithoutClaimingTheSessionIsGone(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		response := serve(t, daemon.handler, method, "/api/sessions/"+info.ID+"/pin", nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /pin status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
	// The correct verb on a session that really is missing still says so.
	missing := serve(t, daemon.handler, http.MethodPut, "/api/sessions/missing/pin",
		strings.NewReader(`{"pinned":true}`), "127.0.0.1:4321", nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("PUT /pin on a missing session status=%d body=%s", missing.Code, missing.Body.String())
	}
}
