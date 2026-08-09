package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/migrate"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestMigrateCreateStartsOneInteractiveClaudeTargetAndRecordsSourceLineage(t *testing.T) {
	daemon := newTestDaemon(t)
	ledgerPath := filepath.Join(daemon.root, "ledger", "lanes.sqlite3")
	t.Setenv("SESSIONS_LEDGER_PATH", ledgerPath)
	provider := "11111111-1111-4111-8111-111111111111"
	encoded, err := json.Marshal(migrate.CreateRequest{
		Tool: "claude-code", UUID: provider, Cwd: daemon.root,
		ResumeRecipe: []string{"claude", "--resume", provider},
		Name:         "Continue BOLO", SourceID: "source-lane",
		SourceEndpoint: "http://source-machine",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/migrate/create",
		bytes.NewReader(encoded), "198.51.100.20:7000",
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}},
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result migrate.CreateResult
	decodeBody(t, response, &result)
	if result.Session.ID == "" || result.Session.Kind != "" ||
		result.Session.ConversationID != provider || result.Session.ClaudeSessionID != provider ||
		!result.LineageRecorded {
		t.Fatalf("create result = %#v", result)
	}
	t.Cleanup(func() {
		_ = daemon.registry.RequestKill(context.Background(), result.Session.ID, false)
	})

	store, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.Events(context.Background(), result.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := ledger.Fold(events)
	if len(states) != 1 ||
		states[0].MovedFromMachine != "http://source-machine" ||
		states[0].MovedFromLaneID != "source-lane" {
		t.Fatalf("target lineage = %#v", states)
	}
}

func TestMigrateExportRefusesLiveSourceWithoutChangingIt(t *testing.T) {
	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.registry.RequestKill(context.Background(), created.ID, false) })
	encoded, _ := json.Marshal(migrate.ExportRequest{SessionID: created.ID, SourceEndpoint: "http://source"})
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/migrate/export",
		bytes.NewReader(encoded), "198.51.100.20:7000",
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}},
	)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte("still live")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current, ok := daemon.registry.Get(created.ID)
	if !ok || current.Info().Exited {
		t.Fatal("export changed the live source")
	}
}

// TestMigrateReceiveEnforcesTheSameRequestGuardsAsEveryOtherPost pins the
// route that decodes with its own decoder to the guards readJSON applies. It
// used to accept any content type and any byte sequence.
func TestMigrateReceiveEnforcesTheSameRequestGuardsAsEveryOtherPost(t *testing.T) {
	daemon := newTestDaemon(t)
	remote := "198.51.100.20:7000"
	authorized := http.Header{"Authorization": {"Bearer " + testToken}}

	wrongType := http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"text/plain"}}
	response := serve(t, daemon.handler, http.MethodPost, "/api/migrate/receive",
		bytes.NewReader([]byte(`{"tool":"claude-code"}`)), remote, wrongType)
	if response.Code != http.StatusUnsupportedMediaType ||
		!bytes.Contains(response.Body.Bytes(), []byte("content-type must be application/json")) {
		t.Fatalf("wrong content type: status=%d body=%s", response.Code, response.Body.String())
	}

	twoTypes := http.Header{
		"Authorization": {"Bearer " + testToken},
		"Content-Type":  {"application/json", "application/json"},
	}
	response = serve(t, daemon.handler, http.MethodPost, "/api/migrate/receive",
		bytes.NewReader([]byte(`{"tool":"claude-code"}`)), remote, twoTypes)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("two content types: status=%d body=%s", response.Code, response.Body.String())
	}

	invalidUTF8 := append([]byte(`{"tool":"`), 0xff, 0xfe)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	response = serve(t, daemon.handler, http.MethodPost, "/api/migrate/receive",
		bytes.NewReader(invalidUTF8), remote,
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}})
	if response.Code != http.StatusBadRequest ||
		!bytes.Contains(response.Body.Bytes(), []byte("valid UTF-8")) {
		t.Fatalf("invalid UTF-8: status=%d body=%s", response.Code, response.Body.String())
	}

	// A well-formed request still reaches migrate.Receive and fails on its own
	// terms rather than on a transport guard.
	response = serve(t, daemon.handler, http.MethodPost, "/api/migrate/receive",
		bytes.NewReader([]byte(`{"tool":"claude-code"}`)), remote,
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}})
	if response.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("valid request rejected as unsupported media type: body=%s", response.Body.String())
	}
	_ = authorized
}

// TestIntegrationBytesAndJSONAgreeOnCORS keeps one CORS answer across response
// helpers: a browser preflighting a transcript download must not learn a
// different allowed-method or allowed-header set than any JSON route gives.
func TestIntegrationBytesAndJSONAgreeOnCORS(t *testing.T) {
	daemon := newTestDaemon(t)
	origin := "https://sessions.somewhere.tech"

	jsonResponse := httptest.NewRecorder()
	daemon.handler.sendJSON(jsonResponse, http.StatusOK, map[string]any{"ok": true}, origin)
	bytesResponse := httptest.NewRecorder()
	daemon.handler.sendIntegrationBytes(bytesResponse, http.StatusOK, "text/plain; charset=utf-8", []byte("x"), origin)

	for _, header := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Vary",
	} {
		if got, want := bytesResponse.Header().Get(header), jsonResponse.Header().Get(header); got != want {
			t.Errorf("%s = %q on byte responses and %q on JSON responses", header, got, want)
		}
	}
	if got := bytesResponse.Header().Get("Access-Control-Expose-Headers"); got != "X-Sessions-Schema-Version" {
		t.Errorf("byte responses stopped exposing the schema version header: %q", got)
	}
}

func TestMigrateCompleteRejectsInvalidTargetEndpoint(t *testing.T) {
	daemon := newTestDaemon(t)
	encoded, _ := json.Marshal(migrate.CompleteRequest{
		SourceID: "source", TargetID: "target", TargetEndpoint: "file:///tmp/not-a-daemon",
	})
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/migrate/complete",
		bytes.NewReader(encoded), "198.51.100.20:7000",
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}},
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMigrateCompleteRequiresKnownEndedSource(t *testing.T) {
	daemon := newTestDaemon(t)
	encoded, _ := json.Marshal(migrate.CompleteRequest{
		SourceID: "missing-source", TargetID: "target", TargetEndpoint: "https://target.example.ts.net",
	})
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/migrate/complete",
		bytes.NewReader(encoded), "198.51.100.20:7000",
		http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}},
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
