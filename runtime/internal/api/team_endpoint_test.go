package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type teamReadRegistry struct {
	*state.Registry
	infos []state.SessionInfo
	err   error
}

func (r *teamReadRegistry) TeamSnapshot(context.Context) ([]state.SessionInfo, error) {
	return r.infos, r.err
}

func TestTeamEndpointDoesNotTurnReadFailureIntoEmptySuccess(t *testing.T) {
	daemon := newTestDaemon(t)
	registry := &teamReadRegistry{Registry: daemon.registry, err: errors.New("ledger unavailable")}
	handler := New(daemon.config, registry)
	response := serve(t, handler, http.MethodGet, "/api/lanes/mine?lane=manager", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("read failure = %d %s", response.Code, response.Body.String())
	}
}

func TestTeamEndpointBaselineDeltaAndExpiredCursor(t *testing.T) {
	daemon := newTestDaemon(t)
	registry := &teamReadRegistry{Registry: daemon.registry, infos: []state.SessionInfo{
		lane("manager", "", "shell"), lane("worker", "manager", "codex"),
	}}
	handler := New(daemon.config, registry)
	baseline := serve(t, handler, http.MethodGet, "/api/lanes/mine?lane=manager", nil, "127.0.0.1:1", nil)
	var listing teamListing
	decodeBody(t, baseline, &listing)
	if baseline.Code != http.StatusOK || listing.NextCursor == "" || len(listing.Members) != 1 {
		t.Fatalf("baseline = %d %s", baseline.Code, baseline.Body.String())
	}
	cursor := listing.NextCursor
	quiet := serve(t, handler, http.MethodGet, "/api/lanes/mine?lane=manager&since="+cursor, nil, "127.0.0.1:1", nil)
	listing = teamListing{}
	decodeBody(t, quiet, &listing)
	if quiet.Code != http.StatusOK || !listing.Delta || len(listing.Members) != 0 || listing.Total != 1 {
		t.Fatalf("quiet delta = %d %s", quiet.Code, quiet.Body.String())
	}
	registry.infos[1].LastSummary = "New result"
	changed := serve(t, handler, http.MethodGet, "/api/lanes/mine?lane=manager&since="+cursor, nil, "127.0.0.1:1", nil)
	listing = teamListing{}
	decodeBody(t, changed, &listing)
	if changed.Code != http.StatusOK || len(listing.Members) != 1 || listing.Members[0].Summary != "New result" {
		t.Fatalf("changed delta = %d %s", changed.Code, changed.Body.String())
	}
	invalid := serve(t, handler, http.MethodGet, "/api/lanes/mine?lane=manager&since=expired", nil, "127.0.0.1:1", nil)
	if invalid.Code != http.StatusConflict {
		t.Fatalf("invalid cursor = %d %s", invalid.Code, invalid.Body.String())
	}
}
