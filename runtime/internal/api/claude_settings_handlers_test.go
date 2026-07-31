package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestClaudeSettingsRoundTrip(t *testing.T) {
	daemon := newTestDaemon(t)

	response := serve(t, daemon.handler, http.MethodGet, "/api/claude/settings", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	var defaults state.ClaudeSettings
	decodeBody(t, response, &defaults)
	if defaults.PermissionMode != state.ClaudeChoiceInherit || defaults.RemoteControl != state.ClaudeChoiceOff {
		t.Fatalf("defaults = %#v", defaults)
	}
	consentHeaders := http.Header{remoteControlConsentHeader: {"remote-control"}}
	response = serve(t, daemon.handler, http.MethodPut, "/api/onboarding",
		bytes.NewBufferString(`{"remoteControl":"enabled"}`), "127.0.0.1:1", consentHeaders)
	if response.Code != http.StatusOK {
		t.Fatalf("onboarding PUT status=%d body=%s", response.Code, response.Body.String())
	}

	want := state.ClaudeSettings{
		RemoteControl: state.ClaudeChoiceOn, PermissionMode: state.ClaudePermissionManual,
		Model: "opus", Effort: "high", Chrome: state.ClaudeChoiceOff,
		SomewhereMCP: state.ClaudeSomewhereEnsure, RemoteControlNamePrefix: "sessions",
	}
	encoded, _ := json.Marshal(want)
	response = serve(t, daemon.handler, http.MethodPut, "/api/claude/settings", bytes.NewReader(encoded), "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body.String())
	}
	var saved state.ClaudeSettings
	decodeBody(t, response, &saved)
	if saved != want {
		t.Fatalf("saved = %#v, want %#v", saved, want)
	}

	response = serve(t, daemon.handler, http.MethodPut, "/api/claude/settings", bytes.NewBufferString(`{"permissionMode":"unsafe"}`), "127.0.0.1:1", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status=%d body=%s", response.Code, response.Body.String())
	}
}
