package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestWaitStateReportsClassifierEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		cmd    string
		source string
	}{
		{name: "structured codex", cmd: "codex", source: "structured"},
		{name: "terminal heuristic", cmd: "/bin/sh", source: "heuristic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			daemon := newTestDaemon(t)
			info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: test.cmd, Cwd: daemon.root})
			if err != nil {
				t.Fatal(err)
			}
			session, ok := daemon.registry.Get(info.ID)
			if !ok {
				t.Fatal("created session missing")
			}
			session.SetWorking(true)
			response := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/wait", nil, "127.0.0.1:1", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var body map[string]any
			decodeBody(t, response, &body)
			assertExactKeys(t, body, "cwd", "session", "source", "working")
			if body["session"] != info.ID || body["cwd"] != daemon.root || body["working"] != true || body["source"] != test.source {
				t.Fatalf("unexpected wait state: %#v", body)
			}
		})
	}
}

func TestWaitStateUnknownSession(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(t, daemon.handler, http.MethodGet, "/api/sessions/missing/wait-state", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
