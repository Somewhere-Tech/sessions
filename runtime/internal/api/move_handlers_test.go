package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/migrate"
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
		result.Session.ConversationID != "" || !result.LineageRecorded {
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
