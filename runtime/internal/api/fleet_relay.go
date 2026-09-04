package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/fleetendpoint"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

const (
	fleetRegistryVersion = 1
	fleetProbeTimeout    = 7 * time.Second
	fleetRegistryLimit   = 256 * 1024
)

type fleetSavedMachine struct {
	MachineID         string `json:"machine_id"`
	Name              string `json:"name"`
	Endpoint          string `json:"endpoint"`
	LANEndpoint       string `json:"lan_endpoint,omitempty"`
	TailnetEndpoint   string `json:"tailnet_endpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnet_ip_endpoint,omitempty"`
	Transport         string `json:"transport"`
}

type fleetMachineRegistry struct {
	Version  int                 `json:"version"`
	Machines []fleetSavedMachine `json:"machines"`
}

type fleetMachineView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Endpoint          string `json:"endpoint"`
	Transport         string `json:"transport"`
	LANEndpoint       string `json:"lan_endpoint,omitempty"`
	TailnetEndpoint   string `json:"tailnet_endpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnet_ip_endpoint,omitempty"`
	Reachable         bool   `json:"reachable"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
}

var fleetRelayTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          32,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

// handleFleetRelay lets this user's paired phone inherit the approved-machine
// set of the host it paired with. It is deliberately a user-owned host relay,
// never a Somewhere-hosted relay: every destination is a machine for which
// this host already holds that machine's independently revocable credential.
func (s *Server) handleFleetRelay(
	response http.ResponseWriter,
	request *http.Request,
	corsOrigin string,
) bool {
	if request.URL.Path != "/api/fleet/machines" &&
		!strings.HasPrefix(request.URL.Path, "/api/fleet/") {
		return false
	}
	principal, _ := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	deviceID, allowed := fleetRelayCaller(principal)
	if !allowed {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "fleet relay requires a local caller or paired device credential",
		}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/fleet/machines" {
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		s.serveFleetMachines(response, request, corsOrigin)
		return true
	}

	machineID, remotePath, ok := parseFleetRelayPath(request.URL.Path)
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": request.URL.Path}, corsOrigin)
		return true
	}
	machine, credential, err := s.approvedFleetMachine(machineID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errFleetMachineNotApproved) {
			status = http.StatusNotFound
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	target, selected, err := s.selectFleetEndpoint(request.Context(), machine, credential)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}

	log.Printf("sessionsd: fleet relay method=%s path=%s machine=%s device_id=%s", request.Method, remotePath, machine.MachineID, deviceID)
	proxy := &httputil.ReverseProxy{
		Transport:     fleetRelayTransport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(target)
			proxyRequest.Out.URL.Path = remotePath
			proxyRequest.Out.URL.RawPath = ""
			query := proxyRequest.Out.URL.Query()
			query.Del("token")
			proxyRequest.Out.URL.RawQuery = query.Encode()
			proxyRequest.Out.Header.Del("Authorization")
			proxyRequest.Out.Header.Del("Proxy-Authorization")
			proxyRequest.Out.Header.Set("Authorization", "Bearer "+credential)
			// SetXForwarded first removes caller-supplied forwarding claims, then
			// records the phone as the peer. This also keeps a second scratch
			// daemon on loopback from mistaking the relay for ambient local trust.
			proxyRequest.SetXForwarded()
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
			log.Printf("sessionsd: fleet relay failed machine=%s device_id=%s: %v", machine.MachineID, deviceID, proxyErr)
			explained := localnetwork.Explain(selected.Endpoint, proxyErr)
			if localnetwork.IsPermissionError(explained) {
				s.lan.markPermission("denied")
			}
			s.sendJSON(writer, http.StatusBadGateway, map[string]any{"error": explained.Error()}, corsOrigin)
		},
	}
	proxy.ServeHTTP(response, request)
	return true
}

func fleetRelayCaller(principal authPrincipal) (string, bool) {
	if principal.Local {
		return "local", true
	}
	const prefix = "device:"
	if strings.HasPrefix(principal.ID, prefix) && strings.TrimPrefix(principal.ID, prefix) != "" {
		return strings.TrimPrefix(principal.ID, prefix), true
	}
	return "", false
}

func parseFleetRelayPath(path string) (string, string, bool) {
	remainder := strings.TrimPrefix(path, "/api/fleet/")
	parts := strings.SplitN(remainder, "/", 2)
	if len(parts) != 2 || !validFleetMachineID(parts[0]) {
		return "", "", false
	}
	remotePath := "/" + parts[1]
	if !strings.HasPrefix(remotePath, "/api/") && remotePath != "/ws" {
		return "", "", false
	}
	return parts[0], remotePath, true
}

func (s *Server) serveFleetMachines(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	machines, err := s.readFleetMachines()
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": "read approved machines: " + err.Error()}, corsOrigin)
		return
	}
	views := make([]fleetMachineView, len(machines))
	done := make(chan struct{}, len(machines))
	for index, machine := range machines {
		index, machine := index, machine
		views[index] = fleetMachineView{
			ID: machine.MachineID, Name: machine.Name,
			LANEndpoint: machine.LANEndpoint, TailnetEndpoint: machine.TailnetEndpoint,
			TailnetIPEndpoint: machine.TailnetIPEndpoint,
		}
		go func() {
			views[index] = s.fleetMachineReachability(request.Context(), machine, views[index])
			done <- struct{}{}
		}()
	}
	for range machines {
		<-done
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"machines": views}, corsOrigin)
}

func (s *Server) fleetMachineReachability(parent context.Context, machine fleetSavedMachine, view fleetMachineView) fleetMachineView {
	credential, err := s.fleetMachineCredential(machine.MachineID)
	if err != nil || credential == "" {
		return view
	}
	ctx, cancel := context.WithTimeout(parent, fleetProbeTimeout)
	defer cancel()
	target, selected, err := s.selectFleetEndpoint(ctx, machine, credential)
	if err == nil {
		err = probeFleetEndpoint(ctx, target, credential, machine.MachineID)
	}
	if err != nil {
		explained := localnetwork.Explain(machine.Endpoint, err)
		if localnetwork.IsPermissionError(explained) {
			s.lan.markPermission("denied")
			view.Reason, view.Message = localnetwork.Reason, localnetwork.Message
		}
		return view
	}
	view.Endpoint, view.Transport, view.Reachable = selected.Endpoint, selected.Transport, true
	return view
}

func (s *Server) selectFleetEndpoint(ctx context.Context, machine fleetSavedMachine, credential string) (*url.URL, fleetendpoint.Candidate, error) {
	candidates, err := validatedFleetEndpoints(machine)
	if err != nil {
		return nil, fleetendpoint.Candidate{}, err
	}
	if len(candidates) == 1 {
		target, _ := url.Parse(candidates[0].Endpoint)
		return target, candidates[0], nil
	}
	var lastErr error
	for index, candidate := range candidates {
		target, _ := url.Parse(candidate.Endpoint)
		if err := probeFleetEndpoint(ctx, target, credential, machine.MachineID); err == nil {
			return target, candidate, nil
		} else {
			lastErr = err
			explained := localnetwork.Explain(candidate.Endpoint, err)
			if index == 0 && localnetwork.IsPermissionError(explained) {
				s.lan.markPermission("denied")
				s.logLANFallbackOnce(candidates[1:])
			}
		}
	}
	return nil, fleetendpoint.Candidate{}, lastErr
}

func probeFleetEndpoint(ctx context.Context, target *url.URL, credential, machineID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String()+"/api/machine", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := fleetRelayTransport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("machine health returned HTTP %d", response.StatusCode)
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

var errFleetMachineNotApproved = errors.New("machine is not approved on this host")

func (s *Server) approvedFleetMachine(machineID string) (fleetSavedMachine, string, error) {
	if !validFleetMachineID(machineID) {
		return fleetSavedMachine{}, "", errFleetMachineNotApproved
	}
	machines, err := s.readFleetMachines()
	if err != nil {
		return fleetSavedMachine{}, "", fmt.Errorf("read approved machines: %w", err)
	}
	for _, machine := range machines {
		if machine.MachineID != machineID {
			continue
		}
		credential, err := s.fleetMachineCredential(machineID)
		if err != nil {
			return fleetSavedMachine{}, "", fmt.Errorf("read approved machine credential: %w", err)
		}
		if credential == "" {
			return fleetSavedMachine{}, "", errFleetMachineNotApproved
		}
		return machine, credential, nil
	}
	return fleetSavedMachine{}, "", errFleetMachineNotApproved
}

func (s *Server) readFleetMachines() ([]fleetSavedMachine, error) {
	path := filepath.Join(s.fleetStateRoot(), "clients.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []fleetSavedMachine{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var registry fleetMachineRegistry
	decoder := json.NewDecoder(io.LimitReader(file, fleetRegistryLimit))
	if err := decoder.Decode(&registry); err != nil {
		return nil, err
	}
	if registry.Version != fleetRegistryVersion {
		return nil, fmt.Errorf("unsupported saved-machine registry version %d", registry.Version)
	}
	for _, machine := range registry.Machines {
		if !validFleetMachineID(machine.MachineID) {
			return nil, fmt.Errorf("saved machine has invalid id %q", machine.MachineID)
		}
		if _, err := validatedFleetEndpoints(machine); err != nil {
			return nil, err
		}
	}
	return registry.Machines, nil
}

func (s *Server) fleetStateRoot() string {
	if s.config.UserStateRoot != "" {
		return s.config.UserStateRoot
	}
	return s.config.StateRoot
}

func (s *Server) fleetMachineCredential(machineID string) (string, error) {
	if !validFleetMachineID(machineID) {
		return "", errFleetMachineNotApproved
	}
	return tokenstore.ReadSecret(filepath.Join(s.fleetStateRoot(), "clients", machineID+".token"))
}

func validatedFleetEndpoints(machine fleetSavedMachine) ([]fleetendpoint.Candidate, error) {
	lan, tailnet, tailnetIP := machine.LANEndpoint, machine.TailnetEndpoint, machine.TailnetIPEndpoint
	switch fleetTransportName(machine.Transport) {
	case "lan":
		if lan == "" {
			lan = machine.Endpoint
		}
	case "tailnet":
		if tailnet == "" {
			tailnet = machine.Endpoint
		}
	case "tailnet-ip":
		if tailnetIP == "" {
			tailnetIP = machine.Endpoint
		}
	}
	candidates := fleetendpoint.Ordered(lan, tailnet, tailnetIP)
	for _, candidate := range candidates {
		if err := validateFleetCandidate(machine.MachineID, candidate); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("saved machine %q has no endpoint", machine.MachineID)
	}
	return candidates, nil
}

func validateFleetCandidate(machineID string, candidate fleetendpoint.Candidate) error {
	parsed, err := url.Parse(strings.TrimSpace(candidate.Endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("saved machine %q has an invalid endpoint", machineID)
	}
	valid := candidate.Transport == "lan" && parsed.Scheme == "http"
	valid = valid || candidate.Transport == "tailnet" && parsed.Scheme == "https" && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".ts.net")
	valid = valid || candidate.Transport == "tailnet-ip" && parsed.Scheme == "http" && tailscale.TailnetIPv4([]string{parsed.Hostname()}) != ""
	if !valid {
		return fmt.Errorf("saved machine %q has an invalid %s transport", machineID, candidate.Transport)
	}
	return nil
}

func fleetTransportName(value string) string {
	if value == "nearby" {
		return "lan"
	}
	return value
}

func validFleetMachineID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." ||
		strings.Contains(value, "..") || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
