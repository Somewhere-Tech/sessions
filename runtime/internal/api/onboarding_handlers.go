package api

import (
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const remoteControlConsentHeader = "X-Sessions-User-Consent"

type onboardingPreferenceRequest struct {
	RemoteControl string `json:"remoteControl"`
}

func (s *Server) handleOnboardingRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/onboarding" {
		return false
	}
	switch request.Method {
	case http.MethodGet:
		settings, err := state.LoadSettings(s.lan.settingsPath)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, settings.EffectiveOnboarding(), corsOrigin)
	case http.MethodPut:
		if request.Header.Get(remoteControlConsentHeader) != "remote-control" {
			s.sendJSON(response, http.StatusForbidden, map[string]any{
				"error": "this preference requires an explicit user choice in a Sessions user interface",
			}, corsOrigin)
			return true
		}
		var requested onboardingPreferenceRequest
		if err := readJSON(request, &requested); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if requested.RemoteControl != state.RemoteControlConsentEnabled &&
			requested.RemoteControl != state.RemoteControlConsentLocalOnly {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{
				"error": "remoteControl must be enabled or local-only",
			}, corsOrigin)
			return true
		}
		if err := state.UpdateSettings(s.lan.settingsPath, func(settings *state.Settings) error {
			settings.Onboarding = &state.OnboardingSettings{
				Version:              state.OnboardingCurrentVersion,
				RemoteControlConsent: requested.RemoteControl,
			}
			claude := settings.EffectiveClaude()
			if settings.Claude != nil {
				claude = *settings.Claude
			}
			if requested.RemoteControl == state.RemoteControlConsentEnabled {
				claude.RemoteControl = state.ClaudeChoiceOn
			} else {
				claude.RemoteControl = state.ClaudeChoiceOff
			}
			normalized, err := state.NormalizeClaudeSettings(claude)
			if err != nil {
				return err
			}
			settings.Claude = &normalized
			return nil
		}); err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		settings, err := state.LoadSettings(s.lan.settingsPath)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, settings.EffectiveOnboarding(), corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}
