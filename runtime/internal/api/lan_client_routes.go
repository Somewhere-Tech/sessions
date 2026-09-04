package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/discovery"
	"github.com/somewhere-tech/sessions/runtime/internal/fleetendpoint"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

type lanDiscoveredMachine struct {
	discovery.Candidate
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SessionsLoaded int    `json:"sessions_loaded"`
	Reachable      bool   `json:"reachable"`
}

type lanNearbyHealth struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	SessionsLoaded int    `json:"sessionsLoaded"`
	System         struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	} `json:"system"`
}

func (s *Server) handleLANClientRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	switch request.URL.Path {
	case "/api/lan/discover":
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		if !s.requireLocalPrincipal(response, request, corsOrigin, "LAN discovery") {
			return true
		}
		s.serveLANDiscover(response, request, corsOrigin)
		return true
	case "/api/lan/connect":
		if request.Method != http.MethodPost {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		if !s.requireLocalPrincipal(response, request, corsOrigin, "machine connection") {
			return true
		}
		s.serveLANConnect(response, request, corsOrigin)
		return true
	default:
		return false
	}
}

func (s *Server) serveLANDiscover(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	timeout := 3 * time.Second
	if raw := request.URL.Query().Get("timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 || parsed > 15*time.Second {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "timeout must be between 1ns and 15s"}, corsOrigin)
			return
		}
		timeout = parsed
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout+time.Second)
	defer cancel()
	candidates, err := s.lan.browse(ctx, timeout)
	if err != nil {
		s.sendLANClientError(response, err, corsOrigin)
		return
	}
	machines := s.verifiedLANCandidates(ctx, candidates)
	if len(machines) == 0 && s.lan.state().Bonjour.Advertised && initialLocalNetworkPermission() != "not-required" {
		s.lan.markPermission("denied")
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": localnetwork.Message, "reason": localnetwork.Reason,
		}, corsOrigin)
		return
	}
	if len(machines) > 0 {
		s.lan.markPermission("granted")
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"machines": machines,
		"warning":  "Nearby access uses unencrypted HTTP. Connect only on a private network you trust.",
	}, corsOrigin)
}

func (s *Server) verifiedLANCandidates(ctx context.Context, candidates []discovery.Candidate) []lanDiscoveredMachine {
	machines := make([]lanDiscoveredMachine, 0, len(candidates))
	for _, candidate := range candidates {
		health, err := fetchLANHealth(ctx, candidate.Endpoint, "")
		if err != nil || !health.OK || health.Name != "sessionsd" {
			continue
		}
		machines = append(machines, lanDiscoveredMachine{
			Candidate: candidate, Version: health.Version, OS: health.System.OS,
			Arch: health.System.Arch, SessionsLoaded: health.SessionsLoaded, Reachable: true,
		})
	}
	return machines
}

type lanConnectRequest struct {
	Endpoint          string `json:"endpoint"`
	LANEndpoint       string `json:"lan_endpoint"`
	TailnetEndpoint   string `json:"tailnet_endpoint"`
	TailnetIPEndpoint string `json:"tailnet_ip_endpoint"`
	ClientID          string `json:"client_id"`
	Name              string `json:"name"`
	Timeout           string `json:"timeout"`
}

func (s *Server) serveLANConnect(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	var body lanConnectRequest
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	candidates, err := lanConnectCandidates(body)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	timeout := 10 * time.Minute
	if body.Timeout != "" {
		timeout, err = time.ParseDuration(body.Timeout)
		if err != nil || timeout <= 0 || timeout > 10*time.Minute {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "timeout must be between 1ns and 10m"}, corsOrigin)
			return
		}
	}
	claim, used, err := s.connectLANMachineCandidates(request.Context(), candidates, body.ClientID, body.Name, timeout)
	if err != nil {
		s.sendLANClientError(response, err, corsOrigin)
		return
	}
	if used.Transport == "lan" {
		s.lan.markPermission("granted")
	}
	s.sendJSON(response, http.StatusCreated, map[string]any{
		"claim": claim, "endpoint": used.Endpoint, "transport": used.Transport,
	}, corsOrigin)
}

func lanConnectCandidates(body lanConnectRequest) ([]fleetendpoint.Candidate, error) {
	if body.Endpoint != "" {
		endpoint, transport, err := validateLANClientEndpoint(body.Endpoint)
		if err != nil {
			return nil, err
		}
		switch transport {
		case "nearby":
			body.LANEndpoint = endpoint
		case "tailnet":
			body.TailnetEndpoint = endpoint
		case "tailnet-ip":
			body.TailnetIPEndpoint = endpoint
		}
	}
	ordered := fleetendpoint.Ordered(body.LANEndpoint, body.TailnetEndpoint, body.TailnetIPEndpoint)
	for index := range ordered {
		validated, transport, err := validateLANClientEndpoint(ordered[index].Endpoint)
		if err != nil || fleetTransportName(transport) != ordered[index].Transport {
			return nil, fmt.Errorf("invalid %s endpoint", ordered[index].Transport)
		}
		ordered[index].Endpoint = validated
	}
	if len(ordered) == 0 {
		return nil, errors.New("at least one machine endpoint is required")
	}
	return ordered, nil
}

func (s *Server) connectLANMachineCandidates(ctx context.Context, candidates []fleetendpoint.Candidate, clientID, name string, timeout time.Duration) (pairingClaimResponse, fleetendpoint.Candidate, error) {
	selected, err := s.selectLANConnectCandidate(ctx, candidates)
	if err != nil {
		return pairingClaimResponse{}, fleetendpoint.Candidate{}, err
	}
	transport := selected.Transport
	if transport == "lan" {
		transport = "nearby"
	}
	claim, err := connectLANMachine(ctx, selected.Endpoint, transport, clientID, name, timeout)
	return claim, selected, err
}

func (s *Server) selectLANConnectCandidate(ctx context.Context, candidates []fleetendpoint.Candidate) (fleetendpoint.Candidate, error) {
	var lastErr error
	for index, candidate := range candidates {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		health, err := fetchLANHealth(probeCtx, candidate.Endpoint, "")
		cancel()
		if err == nil && health.OK && health.Name == "sessionsd" {
			return candidate, nil
		}
		if err == nil {
			err = errors.New("endpoint did not identify itself as sessionsd")
		}
		explained := localnetwork.Explain(candidate.Endpoint, err)
		if index == 0 && localnetwork.IsPermissionError(explained) {
			s.logLANFallbackOnce(candidates[1:])
			s.lan.markPermission("denied")
		}
		lastErr = fmt.Errorf("reach %s: %w", candidate.Endpoint, explained)
	}
	return fleetendpoint.Candidate{}, lastErr
}

func (s *Server) sendLANClientError(response http.ResponseWriter, err error, corsOrigin string) {
	status := http.StatusBadGateway
	reason := ""
	if localnetwork.IsPermissionError(err) {
		s.lan.markPermission("denied")
		status, reason = http.StatusForbidden, localnetwork.Reason
	}
	s.sendJSON(response, status, map[string]any{"error": err.Error(), "reason": reason}, corsOrigin)
}

func connectLANMachine(ctx context.Context, endpoint, transport, clientID, name string, timeout time.Duration) (pairingClaimResponse, error) {
	requestPath, claimPath := "/api/tailnet/access/request", "/api/tailnet/access/claim"
	if transport == "nearby" {
		requestPath, claimPath = "/api/lan/access/request", "/api/lan/access/claim"
	}
	var created tailnetAccessRequestResponse
	status, err := postLANClientJSON(ctx, endpoint+requestPath, map[string]string{
		"client_id": clientID, "name": name,
	}, &created)
	if err != nil || status != http.StatusAccepted || created.RequestID == "" || created.RequestSecret == "" {
		return pairingClaimResponse{}, firstLANConnectError(err, status, "access request")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var claim pairingClaimResponse
		status, err = postLANClientJSON(ctx, endpoint+claimPath, map[string]string{
			"request_id": created.RequestID, "request_secret": created.RequestSecret,
		}, &claim)
		switch status {
		case http.StatusCreated:
			if err != nil || claim.Token == "" || !validFleetMachineID(claim.MachineID) {
				return pairingClaimResponse{}, firstLANConnectError(err, status, "access claim")
			}
			if err := verifyLANCredential(ctx, endpoint, claim.Token, claim.MachineID); err != nil {
				return pairingClaimResponse{}, fmt.Errorf("verify machine credential: %w", err)
			}
			return claim, nil
		case http.StatusAccepted:
			time.Sleep(2 * time.Second)
		case http.StatusForbidden:
			return pairingClaimResponse{}, errors.New("the other machine denied this access request")
		case http.StatusGone:
			return pairingClaimResponse{}, errors.New("the access request expired; run the command again")
		default:
			return pairingClaimResponse{}, firstLANConnectError(err, status, "access claim")
		}
	}
	return pairingClaimResponse{}, errors.New("access request expired while waiting for approval; run the command again")
}

func firstLANConnectError(err error, status int, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s failed with HTTP %d", operation, status)
}

func postLANClientJSON(ctx context.Context, target string, body, output any) (int, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := fleetRelayTransport.RoundTrip(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if response.StatusCode >= 400 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error != "" {
			return response.StatusCode, errors.New(failure.Error)
		}
		return response.StatusCode, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if output != nil && len(payload) > 0 {
		err = json.Unmarshal(payload, output)
	}
	return response.StatusCode, err
}

func fetchLANHealth(ctx context.Context, endpoint, token string) (lanNearbyHealth, error) {
	var health lanNearbyHealth
	path := "/api/health"
	if token != "" {
		path = "/api/machine"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
	if err != nil {
		return health, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := fleetRelayTransport.RoundTrip(request)
	if err != nil {
		return health, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return health, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	if token != "" {
		return health, nil
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health)
	return health, err
}

func verifyLANCredential(ctx context.Context, endpoint, token, machineID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/machine", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := fleetRelayTransport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("credential verification returned HTTP %d", response.StatusCode)
	}
	var identity struct {
		MachineID string `json:"machine_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&identity); err != nil {
		return err
	}
	if identity.MachineID != machineID {
		return errors.New("credential verified a different machine identity")
	}
	return nil
}

func validateLANClientEndpoint(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", "", errors.New("endpoint must be a trusted-LAN HTTP origin or Tailscale HTTPS origin")
	}
	parsed.Path = ""
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname()).To4()
		port, portErr := strconv.Atoi(parsed.Port())
		if ip != nil && tailscale.TailnetIPv4([]string{ip.String()}) != "" && portErr == nil && port >= 1024 && port <= 65535 {
			return strings.TrimSuffix(parsed.String(), "/"), "tailnet-ip", nil
		}
		if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) || portErr != nil || port < 1024 || port > 65535 {
			return "", "", errors.New("nearby HTTP endpoints require a private or loopback IPv4 literal and explicit port")
		}
		return strings.TrimSuffix(parsed.String(), "/"), "nearby", nil
	}
	if parsed.Scheme == "https" && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".ts.net") {
		return strings.TrimSuffix(parsed.String(), "/"), "tailnet", nil
	}
	return "", "", errors.New("endpoint scheme must be http for a trusted LAN or https for Tailscale")
}
