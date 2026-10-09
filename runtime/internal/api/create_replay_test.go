package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

// A retried create is answered with the session its operation recorded, even
// when the provider work a new launch needs would now fail. The client must
// see a replay (200, start.replayed), not a refusal that reads as "nothing
// was created".
func TestRetriedCreateIsAReplayEvenWhenTheCatalogFails(t *testing.T) {
	daemon := newTestDaemon(t)
	t.Setenv("HOME", daemon.root)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var mu sync.Mutex
	calls, catalogErr := 0, error(nil)
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		ListCodexModels: func(context.Context, string) ([]codexapp.Model, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if catalogErr != nil {
				return nil, catalogErr
			}
			return []codexapp.Model{{ID: "alpha", Model: "wire-alpha", IsDefault: true}}, nil
		},
	})
	t.Cleanup(manager.Close)
	handler := New(daemon.config, manager)
	post := func(body string) (int, map[string]any) {
		t.Helper()
		response := serve(t, handler, http.MethodPost, "/api/sessions", strings.NewReader(body), "127.0.0.1:1", nil)
		var decoded map[string]any
		decodeBody(t, response, &decoded)
		return response.Code, decoded
	}
	body := `{"cmd":"codex","cwd":"` + daemon.root + `","kind":"codex-app-server","args":["--model","alpha"],` +
		`"operation_id":"` + apiStartOperation + `","prompt_operation_id":"` + apiPromptOperation + `"}`

	code, first := post(body)
	if code != http.StatusCreated {
		t.Fatalf("first create = %d %#v", code, first)
	}
	mu.Lock()
	catalogErr = errors.New("catalog unavailable")
	mu.Unlock()
	code, again := post(body)
	start, _ := again["start"].(map[string]any)
	if code != http.StatusOK || again["id"] != first["id"] || start["replayed"] != true {
		t.Fatalf("retried create = %d %#v, want 200 replay of %v", code, again, first["id"])
	}

	// A malformed id is still refused, and before any provider work.
	code, refused := post(`{"cmd":"codex","cwd":"` + daemon.root + `","kind":"codex-app-server","args":["--model","alpha"],"prompt_operation_id":"NOT-A-UUID"}`)
	mu.Lock()
	defer mu.Unlock()
	if code != http.StatusBadRequest || !strings.Contains(refused["error"].(string), "prompt_operation_id") || calls != 1 {
		t.Fatalf("malformed prompt id = %d %#v, catalog calls %d (want 1, from the first create)", code, refused, calls)
	}
	if len(daemon.launcher.Launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(daemon.launcher.Launches))
	}
}
