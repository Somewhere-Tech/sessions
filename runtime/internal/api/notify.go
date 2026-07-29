package api

import (
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type notifyState struct {
	Notify     state.NotifySettings `json:"notify"`
	Subscribed bool                 `json:"subscribed"`
}

type pushSubscriptionReporter interface {
	HasSubscriptions() bool
}

func (s *Server) handleNotifyRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/notify" {
		return false
	}
	switch request.Method {
	case http.MethodGet:
		current, err := s.notifyState()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, current, corsOrigin)
	case http.MethodPost:
		var body struct {
			Enabled *bool  `json:"enabled"`
			Kind    string `json:"kind"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if body.Enabled == nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "enabled must be true or false"}, corsOrigin)
			return true
		}
		probe := state.DefaultNotifySettings()
		if err := probe.Set(body.Kind, *body.Enabled); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if err := state.UpdateSettings(s.lan.settingsPath, func(settings *state.Settings) error {
			notify := settings.EffectiveNotify()
			if err := notify.Set(body.Kind, *body.Enabled); err != nil {
				return err
			}
			settings.Notify = &notify
			return nil
		}); err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		current, err := s.notifyState()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, current, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}

func (s *Server) notifyState() (notifyState, error) {
	settings, err := state.LoadSettings(s.lan.settingsPath)
	if err != nil {
		return notifyState{}, err
	}
	current := notifyState{Notify: settings.EffectiveNotify()}
	if subscriptions, ok := s.push.(pushSubscriptionReporter); ok {
		current.Subscribed = subscriptions.HasSubscriptions()
	}
	return current, nil
}
