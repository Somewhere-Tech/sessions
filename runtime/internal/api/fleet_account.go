package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/fleetaccount"
)

func (s *Server) initFleetAccount() {
	if s.identityError != nil || s.identity.ID == "" {
		s.accountError = errors.New("machine identity is unavailable")
		return
	}
	accountPath := s.config.FleetAccountPath
	keyPath := s.config.FleetKeyPath
	if accountPath == "" {
		accountPath = filepath.Join(s.config.StateRoot, "fleet-account.json")
	}
	if keyPath == "" {
		keyPath = filepath.Join(s.config.StateRoot, "fleet-machine-key.json")
	}
	s.account, s.accountError = fleetaccount.New(fleetaccount.Options{
		BaseURL: s.config.FleetURL, AccountPath: accountPath, KeyPath: keyPath,
		MachineID: s.identity.ID, MachineName: s.identity.Name, DaemonVersion: Version,
		Endpoints: s.fleetAccountEndpoints,
	})
}

func (s *Server) fleetAccountEndpoints() fleetaccount.Endpoints {
	result := fleetaccount.Endpoints{Relay: s.relayMachineEndpoint()}
	if lan := s.lan.state(); lan.URL != nil {
		result.LAN = *lan.URL
	}
	remote := s.remote.state()
	result.Tailnet = remote.Endpoint
	result.TailnetIP = remote.TailnetIPEndpoint
	return result
}

func (s *Server) fleetAccountHealth() map[string]any {
	if s.accountError != nil || s.account == nil {
		detail := "fleet account is unavailable"
		if s.accountError != nil {
			detail = s.accountError.Error()
		}
		return map[string]any{"signedIn": false, "error": detail}
	}
	return s.account.Health()
}

func (s *Server) StartFleetAccount(ctx context.Context, logf func(string, ...any)) {
	if s.account == nil {
		if s.accountError != nil {
			logf("sessionsd: fleet account unavailable: %v", s.accountError)
		}
		return
	}
	s.account.Start(ctx, logf)
}

func (s *Server) handleFleetAccountRoute(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
) bool {
	if !strings.HasPrefix(request.URL.Path, "/api/account") {
		return false
	}
	if !s.requireLocalPrincipal(response, request, corsOrigin, "Somewhere account administration") {
		return true
	}
	if s.account == nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": s.fleetAccountError()}, corsOrigin)
		return true
	}
	switch request.URL.Path {
	case "/api/account":
		s.serveFleetAccountStatus(response, request, corsOrigin)
	case "/api/account/magic-link":
		s.serveFleetMagicLink(response, request, corsOrigin)
	case "/api/account/verify":
		s.serveFleetMagicVerify(response, request, corsOrigin)
	case "/api/account/logout":
		s.serveFleetLogout(response, request, corsOrigin)
	case "/api/account/key":
		s.serveFleetKey(response, request, corsOrigin)
	case "/api/account/machines":
		s.serveFleetAccountMachines(response, request, corsOrigin)
	case "/api/account/machines/claim":
		s.serveFleetAccountMachineClaim(response, request, corsOrigin)
	default:
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found"}, corsOrigin)
	}
	return true
}

func (s *Server) serveFleetAccountMachines(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	status, err := s.account.Status()
	if err == nil && !status.SignedIn {
		s.sendJSON(response, http.StatusOK, map[string]any{
			"machines": []fleetaccount.Machine{}, "signed_in": false, "machine_id": s.account.MachineID(),
		}, corsOrigin)
		return
	}
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	machines, err := s.account.Machines(request.Context())
	if err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"machines": fleetHostMachines(machines), "signed_in": true, "machine_id": s.account.MachineID(),
	}, corsOrigin)
}

func fleetHostMachines(machines []fleetaccount.Machine) []fleetaccount.Machine {
	result := make([]fleetaccount.Machine, 0, len(machines))
	for _, machine := range machines {
		endpoints := machine.EndpointsJSON
		if endpoints.LAN != "" || endpoints.Tailnet != "" || endpoints.TailnetIP != "" || endpoints.Relay != "" {
			result = append(result, machine)
		}
	}
	return result
}

func (s *Server) serveFleetAccountMachineClaim(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	var body struct {
		MachineID string `json:"machine_id"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	directory, err := s.account.Machines(request.Context())
	machine, found := fleetAccountMachine(directory, strings.TrimSpace(body.MachineID))
	if err == nil && !found {
		err = errors.New("machine is not in this account directory")
	}
	if err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	candidates, err := lanConnectCandidates(lanConnectRequest{
		LANEndpoint: machine.EndpointsJSON.LAN, TailnetEndpoint: machine.EndpointsJSON.Tailnet,
		TailnetIPEndpoint: machine.EndpointsJSON.TailnetIP, RelayEndpoint: machine.EndpointsJSON.Relay,
	})
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	selected, err := s.selectLANConnectCandidate(request.Context(), candidates)
	if err != nil {
		s.sendLANClientError(response, err, corsOrigin)
		return
	}
	claim, err := s.account.CreateAccountClaim(machine.ID)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	var issued pairingClaimResponse
	status, err := postLANClientJSON(request.Context(), selected.Endpoint+fleetaccount.AccountClaimPath, claim, &issued)
	if err != nil || status != http.StatusCreated || issued.Token == "" || issued.MachineID != machine.ID {
		s.sendLANClientError(response, firstLANConnectError(err, status, "account credential claim"), corsOrigin)
		return
	}
	if err := verifyLANCredential(request.Context(), selected.Endpoint, issued.Token, machine.ID); err != nil {
		s.sendLANClientError(response, fmt.Errorf("verify account machine credential: %w", err), corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusCreated, map[string]any{
		"claim": issued, "endpoint": selected.Endpoint, "transport": selected.Transport,
	}, corsOrigin)
}

func fleetAccountMachine(machines []fleetaccount.Machine, id string) (fleetaccount.Machine, bool) {
	for _, machine := range machines {
		if machine.ID == id {
			return machine, true
		}
	}
	return fleetaccount.Machine{}, false
}

func (s *Server) handleFleetAccountClaimRoute(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
) bool {
	if request.URL.Path != fleetaccount.AccountClaimPath {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if !requestHasJSONContentType(request) {
		s.sendJSON(response, http.StatusUnsupportedMediaType, map[string]any{"error": "content-type must be application/json"}, corsOrigin)
		return true
	}
	if s.account == nil {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{"error": "account access is unavailable on this machine"}, corsOrigin)
		return true
	}
	var claim fleetaccount.AccountClaim
	if err := readJSON(request, &claim); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	device, err := s.account.VerifyAccountClaim(request.Context(), claim)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, fleetaccount.ErrClaimInvalid) || errors.Is(err, fleetaccount.ErrClaimExpired) ||
			errors.Is(err, fleetaccount.ErrClaimReplay) || errors.Is(err, fleetaccount.ErrDifferentOwner) {
			status = http.StatusForbidden
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	name := strings.TrimSpace(device.Name)
	if name == "" {
		name = "Sessions device"
	}
	record, token, err := s.pair.devices.createPending(name, time.Now().UTC().Add(tailnetCredentialAckTTL))
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	issued := pairingClaimResponse{DeviceID: record.DeviceID, Token: token, Name: record.Name}
	s.completeAccessClaim(&issued)
	log.Printf("sessionsd: access granted to %s via account", record.Name)
	s.sendJSON(response, http.StatusCreated, issued, corsOrigin)
	return true
}

func (s *Server) fleetAccountError() string {
	if s.accountError != nil {
		return s.accountError.Error()
	}
	return "fleet account is unavailable"
}

func (s *Server) serveFleetAccountStatus(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	status, err := s.account.Status()
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, status, corsOrigin)
}

func (s *Server) serveFleetMagicLink(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if strings.TrimSpace(body.Email) == "" || !strings.Contains(body.Email, "@") {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "enter a valid email address"}, corsOrigin)
		return
	}
	if err := s.account.RequestMagicLink(request.Context(), body.Email); err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
}

func (s *Server) serveFleetMagicVerify(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "enter the code or magic link"}, corsOrigin)
		return
	}
	status, err := s.account.VerifyMagicLink(request.Context(), body.Token)
	if err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, status, corsOrigin)
}

func (s *Server) serveFleetLogout(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	if err := s.account.Logout(request.Context()); err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"ok": true, "signed_in": false}, corsOrigin)
}

func (s *Server) serveFleetKey(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	public, err := s.account.PublicKey()
	if err != nil {
		log.Printf("sessionsd: create fleet machine key: %v", err)
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create machine key: %v", err)}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]string{"public_key": public}, corsOrigin)
}
