package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

// The create body decides the account only when it names one. A child created
// through the API with no account starts on its manager's account, and
// defaultProfile is how a client asks for the default login instead.
func TestCreateBodyAccountChoiceReachesTheDaemon(t *testing.T) {
	daemon := newTestDaemon(t)
	t.Setenv("HOME", daemon.root)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	daemon.config.UserStateRoot = filepath.Join(daemon.root, "user-state")
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(daemon.root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(daemon.config, daemon.launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	handler := New(daemon.config, manager)
	create := func(body string, headers http.Header) (int, map[string]any, map[string]string) {
		t.Helper()
		before := len(daemon.launcher.Launches)
		response := serve(t, handler, http.MethodPost, "/api/sessions", strings.NewReader(body), "127.0.0.1:1", headers)
		var decoded map[string]any
		decodeBody(t, response, &decoded)
		if len(daemon.launcher.Launches) == before {
			return response.Code, decoded, nil
		}
		return response.Code, decoded, daemon.launcher.Launches[len(daemon.launcher.Launches)-1].Env
	}
	cwd := `"cwd":"` + daemon.root + `"`
	code, parent, _ := create(`{"cmd":"claude",`+cwd+`,"profile":"work"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("parent create = %d %#v", code, parent)
	}
	child := http.Header{"X-Sessions-Creator-Session": {parent["id"].(string)}}
	home := filepath.Join(daemon.config.UserStateRoot, "profiles", "claude", "work")

	code, inherited, env := create(`{"cmd":"claude",`+cwd+`}`, child)
	if code != http.StatusCreated || inherited["profile"] != "work" || env["CLAUDE_CONFIG_DIR"] != home {
		t.Fatalf("omitted account = %d %#v CLAUDE_CONFIG_DIR=%q", code, inherited, env["CLAUDE_CONFIG_DIR"])
	}
	code, chosenDefault, env := create(`{"cmd":"claude",`+cwd+`,"defaultProfile":true}`, child)
	if _, named := chosenDefault["profile"]; code != http.StatusCreated || named || env == nil || env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("explicit default = %d %#v env=%#v", code, chosenDefault, env)
	}
	code, refused, env := create(`{"cmd":"claude",`+cwd+`,"profile":"work","defaultProfile":true}`, child)
	if code != http.StatusBadRequest || env != nil || !strings.Contains(refused["error"].(string), "not both") {
		t.Fatalf("both choices = %d %#v launched=%v", code, refused, env != nil)
	}
}
