package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Every surface that answers "when did the user last touch this" reads it from
// this route, so the two principal clocks have to be on the wire — and always
// on it, never omitted, because an agent choosing what to end has to tell "no
// human has spoken here" from "this daemon is too old to say".
func TestSessionListingReportsBothMessagePrincipals(t *testing.T) {
	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root, Name: "Integrator",
	})
	if err != nil {
		t.Fatal(err)
	}

	listing := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("listing status = %d body = %s", listing.Code, listing.Body.String())
	}
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	decodeBody(t, listing, &body)
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions = %#v", body.Sessions)
	}
	for _, key := range []string{"lastUserMessageAt", "lastHumanMessageAt", "lastAgentMessageAt"} {
		value, present := body.Sessions[0][key]
		if !present {
			t.Fatalf("%s is missing from the session listing; absence has to mean "+
				"\"nobody has\", which a missing key cannot say", key)
		}
		if value != nil {
			t.Errorf("%s = %v on an untouched session, want null", key, value)
		}
	}

	daemon.registry.RecordInputPrincipal(created.ID, state.PrincipalHuman, "are you there?")
	daemon.registry.RecordInputPrincipal(created.ID, state.PrincipalAgent, "status please")
	live, ok := daemon.registry.Get(created.ID)
	if !ok {
		t.Fatal("created session is not live")
	}
	info := live.Info()
	if info.LastHumanMessageAt == nil || info.LastAgentMessageAt == nil {
		t.Fatalf("stamps = %v/%v, want both set", info.LastHumanMessageAt, info.LastAgentMessageAt)
	}

	stamped := serve(t, daemon.handler, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	decodeBody(t, stamped, &body)
	if got := body.Sessions[0]["lastHumanMessageAt"]; got != float64(*info.LastHumanMessageAt) {
		t.Errorf("lastHumanMessageAt = %v, want %d", got, *info.LastHumanMessageAt)
	}
	if got := body.Sessions[0]["lastAgentMessageAt"]; got != float64(*info.LastAgentMessageAt) {
		t.Errorf("lastAgentMessageAt = %v, want %d", got, *info.LastAgentMessageAt)
	}
	// The transcript-derived field is untouched by the input boundary; it moves
	// only when a user-role record appears in the provider's own conversation.
	if got := body.Sessions[0]["lastUserMessageAt"]; got != nil {
		t.Errorf("lastUserMessageAt = %v after input alone, want null", got)
	}
}
