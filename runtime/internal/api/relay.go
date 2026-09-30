package api

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/relay"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

var errInsecureRelayURL = errors.New("relay URL must use HTTPS (HTTP is allowed only on loopback for development)")

type relayState struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
	Source    string `json:"source,omitempty"`
}

func (s *Server) StartFleetRelay(ctx context.Context, logf func(string, ...any)) {
	if s.account == nil {
		return
	}
	target := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	s.relayConnector = relay.NewConnector(relay.ConnectorOptions{
		URL: s.resolveRelayBase, Target: target, Authenticate: s.account.SignRelayChallenge,
		Logger: log.Default(), OnConnection: s.setRelayConnected,
	})
	go s.runFleetRelay(ctx, logf)
}

func (s *Server) runFleetRelay(ctx context.Context, logf func(string, ...any)) {
	backoff := time.Second
	for ctx.Err() == nil {
		base, err := s.resolveRelayBase(ctx)
		if err == nil && base == "" {
			if !s.waitRelay(ctx, time.Minute) {
				return
			}
			continue
		}
		if err == nil {
			err = s.relayConnector.RunOnce(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		logf("sessionsd: relay tunnel unavailable; retrying in %s: %v", backoff, err)
		if !s.waitRelay(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Server) resolveRelayBase(ctx context.Context) (string, error) {
	if base, source, err := s.configuredRelayBase(); err != nil || base != "" {
		_ = source
		return base, err
	}
	endpoint := strings.TrimSpace(s.account.DirectoryRelay(ctx))
	if endpoint == "" {
		return "", nil
	}
	base, err := relay.BaseURL(endpoint, s.identity.ID)
	if err != nil || !secureRelayURL(base) {
		if err == nil {
			err = errInsecureRelayURL
		}
		return "", err
	}
	s.relayMu.Lock()
	s.relayDirectoryBase = base
	s.relayMu.Unlock()
	return base, nil
}

func (s *Server) configuredRelayBase() (string, string, error) {
	if configured := strings.TrimSpace(s.config.FleetRelayEndpoint); configured != "" {
		base, err := relay.BaseURL(configured, s.identity.ID)
		if err == nil && !secureRelayURL(base) {
			err = errInsecureRelayURL
		}
		return base, "environment", err
	}
	settings, err := state.LoadSettings(s.config.SettingsPath)
	if err != nil {
		return "", "", err
	}
	if settings.Relay != nil && strings.TrimSpace(settings.Relay.URL) != "" {
		base, err := relay.BaseURL(settings.Relay.URL, s.identity.ID)
		if err == nil && !secureRelayURL(base) {
			err = errInsecureRelayURL
		}
		return base, "settings", err
	}
	return "", "", nil
}

func (s *Server) relayMachineEndpoint() string {
	base, _, _ := s.configuredRelayBase()
	if base == "" {
		s.relayMu.RLock()
		base = s.relayDirectoryBase
		s.relayMu.RUnlock()
	}
	endpoint, _ := relay.MachineEndpoint(base, s.identity.ID)
	return endpoint
}

func (s *Server) handleRelayRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/relay" {
		return false
	}
	if !s.requireLocalPrincipal(response, request, corsOrigin, "Relay settings") {
		return true
	}
	switch request.Method {
	case http.MethodGet:
		base, source, err := s.configuredRelayBase()
		if err == nil && base == "" {
			s.relayMu.RLock()
			base, source = s.relayDirectoryBase, "directory"
			s.relayMu.RUnlock()
		}
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.relayMu.RLock()
		connected := s.relayConnected
		s.relayMu.RUnlock()
		s.sendJSON(response, http.StatusOK, relayState{URL: base, Source: source, Connected: connected}, corsOrigin)
	case http.MethodPut:
		s.updateRelaySettings(response, request, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}

func (s *Server) updateRelaySettings(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	var body state.RelaySettings
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	body.URL = strings.TrimSuffix(strings.TrimSpace(body.URL), "/")
	if body.URL != "" {
		base, err := relay.BaseURL(body.URL, s.identity.ID)
		if err != nil || !secureRelayURL(base) {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": errInsecureRelayURL.Error()}, corsOrigin)
			return
		}
		body.URL = base
	}
	err := state.UpdateSettings(s.config.SettingsPath, func(settings *state.Settings) error {
		settings.Relay = &body
		return nil
	})
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.wakeRelay()
	if s.relayConnector != nil {
		s.relayConnector.Disconnect()
	}
	s.sendJSON(response, http.StatusOK, relayState{URL: body.URL, Source: "settings"}, corsOrigin)
}

func secureRelayURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(parsed.Hostname())
	return parsed.Scheme == "http" && (strings.EqualFold(parsed.Hostname(), "localhost") || ip != nil && ip.IsLoopback())
}

func (s *Server) waitRelay(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.relayWake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Server) wakeRelay() {
	select {
	case s.relayWake <- struct{}{}:
	default:
	}
}

func (s *Server) setRelayConnected(connected bool) {
	s.relayMu.Lock()
	s.relayConnected = connected
	s.relayMu.Unlock()
}
