package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// `sessions rename --auto` is the only way back once a session has been
// renamed by hand, so the route has to carry it: the name it returns is the
// provider's own title when the conversation already has one.
func TestRenameAutoOverHTTPHandsTheNameBackToTheProvider(t *testing.T) {
	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root, Name: "Claude - Projects",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := daemon.registry.Get(created.ID)
	if !ok {
		t.Fatal("created session is not live")
	}
	session.RecordClaudeEvent(json.RawMessage(`{"type":"ai-title","aiTitle":"TexasT"}`))

	renamed := serve(t, daemon.handler, http.MethodPut, "/api/sessions/"+created.ID+"/name",
		strings.NewReader(`{"name":"Texas billing"}`), "127.0.0.1:1", nil)
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"name":"Texas billing"`) {
		t.Fatalf("rename: status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	if source := session.Info().NameSource; source != state.NameSourceExplicit {
		t.Fatalf("name source after rename = %q, want %q", source, state.NameSourceExplicit)
	}

	auto := serve(t, daemon.handler, http.MethodPut, "/api/sessions/"+created.ID+"/name",
		strings.NewReader(`{"auto":true}`), "127.0.0.1:1", nil)
	if auto.Code != http.StatusOK || !strings.Contains(auto.Body.String(), `"name":"TexasT"`) {
		t.Fatalf("rename --auto: status=%d body=%s", auto.Code, auto.Body.String())
	}
	if info := session.Info(); info.Name != "TexasT" || info.NameSource != state.NameSourceProvider {
		t.Fatalf("name/source after --auto = %q/%q, want %q/%q",
			info.Name, info.NameSource, "TexasT", state.NameSourceProvider)
	}
}
