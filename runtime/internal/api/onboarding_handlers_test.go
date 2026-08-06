package api

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestOnboardingRequiresExplicitUserConsentHeader(t *testing.T) {
	daemon := newTestDaemon(t)
	legacyClaude := state.ClaudeSettings{RemoteControl: state.ClaudeChoiceOn}
	if err := state.SaveSettings(daemon.handler.lan.settingsPath, state.Settings{Claude: &legacyClaude}); err != nil {
		t.Fatal(err)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/api/onboarding",
		nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	var initial state.OnboardingState
	decodeBody(t, response, &initial)
	if initial.Complete || initial.RemoteControl != state.RemoteControlConsentPending {
		t.Fatalf("legacy onboarding = %#v", initial)
	}
	claudeResponse := serve(t, daemon.handler, http.MethodGet, "/api/claude/settings",
		nil, "127.0.0.1:4321", nil)
	var effectiveClaude state.ClaudeSettings
	decodeBody(t, claudeResponse, &effectiveClaude)
	if effectiveClaude.RemoteControl != state.ClaudeChoiceOff {
		t.Fatalf("legacy Claude setting bypassed onboarding: %#v", effectiveClaude)
	}

	response = serve(t, daemon.handler, http.MethodPut, "/api/onboarding",
		bytes.NewBufferString(`{"remoteControl":"enabled"}`), "127.0.0.1:4321", nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	settings, err := state.LoadSettings(daemon.handler.lan.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.EffectiveOnboarding(); got.Complete || got.RemoteControl != state.RemoteControlConsentPending {
		t.Fatalf("missing-header onboarding = %#v", got)
	}
}

func TestOnboardingPersistsEnabledAndLocalOnlyChoices(t *testing.T) {
	for _, choice := range []string{
		state.RemoteControlConsentEnabled,
		state.RemoteControlConsentLocalOnly,
	} {
		t.Run(choice, func(t *testing.T) {
			daemon := newTestDaemon(t)
			headers := http.Header{onboardingConsentHeader: {"onboarding"}}
			response := serve(t, daemon.handler, http.MethodPut, "/api/onboarding",
				bytes.NewBufferString(`{"remoteControl":"`+choice+`","delegatedAccess":"autonomous"}`), "127.0.0.1:4321", headers)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var got state.OnboardingState
			decodeBody(t, response, &got)
			if !got.Complete || got.RemoteControl != choice || got.DelegatedAccess != state.DelegatedAccessConsentAutonomous {
				t.Fatalf("onboarding = %#v", got)
			}

			settings, err := state.LoadSettings(daemon.handler.lan.settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantRemoteControl := state.ClaudeChoiceOff
			if choice == state.RemoteControlConsentEnabled {
				wantRemoteControl = state.ClaudeChoiceOn
			}
			if got := settings.EffectiveClaude().RemoteControl; got != wantRemoteControl {
				t.Fatalf("Claude Remote Control = %q, want %q", got, wantRemoteControl)
			}
			if got := settings.EffectiveDelegation().Access; got != state.DelegatedAccessConsentAutonomous {
				t.Fatalf("delegated access = %q, want autonomous", got)
			}
		})
	}
}

func TestClaudeSettingsCannotBypassPendingOnboarding(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(t, daemon.handler, http.MethodPut, "/api/claude/settings",
		bytes.NewBufferString(`{"remoteControl":"on","permissionMode":"inherit","effort":"inherit","chrome":"inherit","somewhereMcp":"inherit"}`),
		"127.0.0.1:4321", nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
