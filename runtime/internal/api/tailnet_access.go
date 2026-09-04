package api

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

const (
	tailnetAccessTTL         = 10 * time.Minute
	tailnetCredentialAckTTL  = 2 * time.Minute
	maxTailnetAccessPending  = 64
	maxTailnetAccessPerLogin = 8
	maxTailnetAccessRetained = 128
)

var (
	errTailnetAccessPending = errors.New("access request is waiting for approval")
	errTailnetAccessDenied  = errors.New("access request was denied")
	errTailnetAccessGone    = errors.New("access request is invalid or expired")
	errTailnetAccessFull    = errors.New("too many access requests are waiting")
)

type tailnetAccessRequest struct {
	ID        string
	Secret    string
	ClientID  string
	Name      string
	Login     string
	UserName  string
	Transport string
	Address   string
	CreatedAt time.Time
	ExpiresAt time.Time
	Status    string
	DeviceID  string
	Token     string
}

type tailnetAccessRequestResponse struct {
	RequestID     string    `json:"request_id"`
	RequestSecret string    `json:"request_secret"`
	ExpiresAt     time.Time `json:"expires_at"`
	Status        string    `json:"status"`
}

type tailnetAccessRequestView struct {
	RequestID string    `json:"request_id"`
	ClientID  string    `json:"client_id"`
	Name      string    `json:"name"`
	Login     string    `json:"login"`
	UserName  string    `json:"user_name,omitempty"`
	Transport string    `json:"transport"`
	Address   string    `json:"address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
}

type tailnetAccessService struct {
	mu       sync.Mutex
	requests map[string]tailnetAccessRequest
	now      func() time.Time
}

func newTailnetAccessService() *tailnetAccessService {
	return &tailnetAccessService{
		requests: make(map[string]tailnetAccessRequest),
		now:      time.Now,
	}
}

func (s *tailnetAccessService) request(identity tailnetIdentity, clientID, name string) (tailnetAccessRequestResponse, error) {
	return s.requestForTransport(identity, clientID, name, "tailnet", "")
}

func (s *tailnetAccessService) requestForTransport(
	identity tailnetIdentity,
	clientID, name, transport, address string,
) (tailnetAccessRequestResponse, error) {
	clientID = strings.TrimSpace(clientID)
	if !validMachineID(clientID) {
		return tailnetAccessRequestResponse{}, errors.New("client_id must be a lowercase v4 UUID")
	}
	name = strings.TrimSpace(name)
	if name != "" && !validIdentityHeader(name, maximumPairingDeviceName) {
		return tailnetAccessRequestResponse{}, errors.New("device name is invalid")
	}
	if name == "" {
		name = truncateDeviceName(identity.Name)
	}
	if name == "" {
		name = "Sessions device"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	for _, existing := range s.requests {
		if existing.Login == identity.Login &&
			existing.Transport == transport &&
			existing.ClientID == clientID &&
			existing.Status != "denied" {
			return tailnetAccessRequestResponse{
				RequestID: existing.ID, RequestSecret: existing.Secret,
				ExpiresAt: existing.ExpiresAt, Status: existing.Status,
			}, nil
		}
	}
	pending := 0
	forLogin := 0
	for _, existing := range s.requests {
		if existing.Status == "pending" {
			pending++
		}
		if existing.Login == identity.Login && existing.Transport == transport && existing.Status != "denied" {
			forLogin++
		}
	}
	if pending >= maxTailnetAccessPending ||
		forLogin >= maxTailnetAccessPerLogin ||
		len(s.requests) >= maxTailnetAccessRetained {
		return tailnetAccessRequestResponse{}, errTailnetAccessFull
	}
	id, err := randomDeviceUUID()
	if err != nil {
		return tailnetAccessRequestResponse{}, err
	}
	secret, err := randomBase64URL(32)
	if err != nil {
		return tailnetAccessRequestResponse{}, err
	}
	expiresAt := now.Add(tailnetAccessTTL)
	request := tailnetAccessRequest{
		ID: id, Secret: secret, ClientID: clientID, Name: name,
		Login: identity.Login, UserName: identity.Name,
		Transport: transport, Address: address,
		CreatedAt: now, ExpiresAt: expiresAt, Status: "pending",
	}
	s.requests[id] = request
	return tailnetAccessRequestResponse{
		RequestID: id, RequestSecret: secret, ExpiresAt: expiresAt, Status: request.Status,
	}, nil
}

func (s *tailnetAccessService) claim(
	identity tailnetIdentity,
	requestID, secret string,
	devices *deviceStore,
) (pairingClaimResponse, error) {
	return s.claimForTransport(identity, "tailnet", requestID, secret, devices)
}

func (s *tailnetAccessService) claimForTransport(
	identity tailnetIdentity,
	transport, requestID, secret string,
	devices *deviceStore,
) (pairingClaimResponse, error) {
	requestID = strings.TrimSpace(requestID)
	secret = strings.TrimSpace(secret)
	if !validMachineID(requestID) || secret == "" || len(secret) > 512 {
		return pairingClaimResponse{}, errTailnetAccessGone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	request, ok := s.requests[requestID]
	if !ok ||
		request.Login != identity.Login ||
		request.Transport != transport ||
		!constantTimeStringEqual(secret, request.Secret) {
		return pairingClaimResponse{}, errTailnetAccessGone
	}
	switch request.Status {
	case "pending":
		return pairingClaimResponse{}, errTailnetAccessPending
	case "denied":
		delete(s.requests, requestID)
		return pairingClaimResponse{}, errTailnetAccessDenied
	case "accepted":
		acknowledgeBy := now.Add(tailnetCredentialAckTTL)
		record, token, err := devices.createPending(request.Name, acknowledgeBy)
		if err != nil {
			return pairingClaimResponse{}, err
		}
		request.DeviceID = record.DeviceID
		request.Token = token
		request.Status = "issued"
		request.ExpiresAt = acknowledgeBy
		s.requests[requestID] = request
		return pairingClaimResponse{DeviceID: record.DeviceID, Token: token, Name: record.Name}, nil
	case "issued":
		return pairingClaimResponse{
			DeviceID: request.DeviceID, Token: request.Token, Name: request.Name,
		}, nil
	default:
		return pairingClaimResponse{}, errTailnetAccessGone
	}
}

func (s *tailnetAccessService) list() []tailnetAccessRequestView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	requests := make([]tailnetAccessRequestView, 0, len(s.requests))
	for _, request := range s.requests {
		if request.Status != "pending" {
			continue
		}
		requests = append(requests, tailnetAccessRequestView{
			RequestID: request.ID, ClientID: request.ClientID, Name: request.Name,
			Login: request.Login, UserName: request.UserName,
			Transport: request.Transport, Address: request.Address,
			CreatedAt: request.CreatedAt, ExpiresAt: request.ExpiresAt, Status: request.Status,
		})
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	return requests
}

func (s *tailnetAccessService) decide(requestID, decision string) (tailnetAccessRequestView, error) {
	requestID = strings.TrimSpace(requestID)
	if !validMachineID(requestID) || (decision != "accept" && decision != "deny") {
		return tailnetAccessRequestView{}, errTailnetAccessGone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	request, ok := s.requests[requestID]
	if !ok {
		return tailnetAccessRequestView{}, errTailnetAccessGone
	}
	if request.Status != "pending" {
		return tailnetAccessRequestView{}, errors.New("access request was already decided")
	}
	if decision == "accept" {
		request.Status = "accepted"
	} else {
		request.Status = "denied"
	}
	s.requests[requestID] = request
	return tailnetAccessRequestView{
		RequestID: request.ID, ClientID: request.ClientID, Name: request.Name,
		Login: request.Login, UserName: request.UserName,
		Transport: request.Transport, Address: request.Address,
		CreatedAt: request.CreatedAt, ExpiresAt: request.ExpiresAt, Status: request.Status,
	}, nil
}

func (s *tailnetAccessService) pruneLocked(now time.Time) {
	for id, request := range s.requests {
		if !now.Before(request.ExpiresAt) {
			delete(s.requests, id)
		}
	}
}

func (s *Server) handleTailnetAccessPublicRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/tailnet/access/request" &&
		request.URL.Path != "/api/tailnet/access/claim" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	// This bootstrap intentionally exists only in signed native clients.
	// Browser JavaScript on a valid tailnet origin would otherwise inherit the
	// same Serve identity and could read the resulting bearer credential.
	if len(request.Header.Values("Origin")) > 0 {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "tailnet access requests are available only in Sessions.app",
		}, "")
		return true
	}
	identity, ok := tailscaleServeIdentity(request)
	if !ok {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "a verified Tailscale Serve identity is required",
		}, corsOrigin)
		return true
	}
	contentTypes := request.Header.Values("Content-Type")
	mediaType := ""
	var err error
	if len(contentTypes) == 1 {
		mediaType, _, err = mime.ParseMediaType(contentTypes[0])
	}
	if len(contentTypes) != 1 || err != nil || mediaType != "application/json" {
		s.sendJSON(response, http.StatusUnsupportedMediaType, map[string]any{
			"error": "content-type must be application/json",
		}, "")
		return true
	}
	if request.URL.Path == "/api/tailnet/access/request" {
		var body struct {
			ClientID string `json:"client_id"`
			Name     string `json:"name"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		created, err := s.tailnetAccess.request(identity, body.ClientID, body.Name)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errTailnetAccessFull) {
				status = http.StatusTooManyRequests
			}
			s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusAccepted, created, corsOrigin)
		return true
	}

	var body struct {
		RequestID     string `json:"request_id"`
		RequestSecret string `json:"request_secret"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	if s.identityError != nil || s.identity.ID == "" {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": "access approval is temporarily unavailable on this machine",
		}, corsOrigin)
		return true
	}
	claimed, err := s.tailnetAccess.claim(identity, body.RequestID, body.RequestSecret, s.pair.devices)
	switch {
	case errors.Is(err, errTailnetAccessPending):
		s.sendJSON(response, http.StatusAccepted, map[string]any{"status": "pending"}, corsOrigin)
	case errors.Is(err, errTailnetAccessDenied):
		s.sendJSON(response, http.StatusForbidden, map[string]any{"status": "denied", "error": err.Error()}, corsOrigin)
	case errors.Is(err, errTailnetAccessGone):
		s.sendJSON(response, http.StatusGone, map[string]any{"status": "expired", "error": err.Error()}, corsOrigin)
	case err != nil:
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
	default:
		s.completeAccessClaim(&claimed)
		s.sendJSON(response, http.StatusCreated, claimed, corsOrigin)
	}
	return true
}

func (s *Server) handleNearbyAccessPublicRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/lan/access/request" &&
		request.URL.Path != "/api/lan/access/claim" {
		return false
	}
	loopback := isLoopbackPeer(request)
	tailnetIP := isTailnetIPRequest(request)
	if !isLANRequest(request) && !tailnetIP && !loopback {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "nearby access is available only on this machine's trusted LAN listener or local loopback",
		}, "")
		return true
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/lan/access/request" && len(request.Header.Values("Origin")) > 0 {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "nearby access requests are available only in Sessions native clients and the sessions CLI",
		}, "")
		return true
	}
	if !requestHasJSONContentType(request) {
		s.sendJSON(response, http.StatusUnsupportedMediaType, map[string]any{
			"error": "content-type must be application/json",
		}, "")
		return true
	}
	identity, transport, address, ok := nearbyAccessIdentity(request, loopback, tailnetIP)
	if !ok {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "nearby access requires a private IPv4 network peer",
		}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/lan/access/request" {
		s.serveNearbyAccessRequest(response, request, corsOrigin, identity, transport, address)
		return true
	}
	s.serveNearbyAccessClaim(response, request, corsOrigin, identity, transport)
	return true
}

func (s *Server) serveNearbyAccessRequest(response http.ResponseWriter, request *http.Request, corsOrigin string, identity tailnetIdentity, transport, address string) {
	var body struct {
		ClientID string `json:"client_id"`
		Name     string `json:"name"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	created, err := s.tailnetAccess.requestForTransport(identity, body.ClientID, body.Name, transport, address)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTailnetAccessFull) {
			status = http.StatusTooManyRequests
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusAccepted, created, corsOrigin)
}

func (s *Server) serveNearbyAccessClaim(response http.ResponseWriter, request *http.Request, corsOrigin string, identity tailnetIdentity, transport string) {
	var body struct {
		RequestID     string `json:"request_id"`
		RequestSecret string `json:"request_secret"`
		Ticket        string `json:"ticket"`
		Name          string `json:"name"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if body.Ticket != "" {
		if len(request.Header.Values("Origin")) > 0 && !sameOriginPairingClaimRequest(request) {
			s.sendJSON(response, http.StatusForbidden, map[string]any{
				"error": "pairing links may be claimed only by Sessions clients or the machine's own pairing page",
			}, "")
			return
		}
		if s.identityError != nil || s.identity.ID == "" {
			s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{
				"error": "pairing is temporarily unavailable on this machine",
			}, corsOrigin)
			return
		}
		s.servePairTicketClaim(response, request, corsOrigin, body.Ticket, body.Name)
		return
	}
	if len(request.Header.Values("Origin")) > 0 {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "nearby access requests are available only in Sessions native clients and the sessions CLI",
		}, "")
		return
	}
	if s.identityError != nil || s.identity.ID == "" {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": "access approval is temporarily unavailable on this machine",
		}, corsOrigin)
		return
	}
	claimed, err := s.tailnetAccess.claimForTransport(identity, transport, body.RequestID, body.RequestSecret, s.pair.devices)
	switch {
	case errors.Is(err, errTailnetAccessPending):
		s.sendJSON(response, http.StatusAccepted, map[string]any{"status": "pending"}, corsOrigin)
	case errors.Is(err, errTailnetAccessDenied):
		s.sendJSON(response, http.StatusForbidden, map[string]any{"status": "denied", "error": err.Error()}, corsOrigin)
	case errors.Is(err, errTailnetAccessGone):
		s.sendJSON(response, http.StatusGone, map[string]any{"status": "expired", "error": err.Error()}, corsOrigin)
	case err != nil:
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
	default:
		s.completeAccessClaim(&claimed)
		s.sendJSON(response, http.StatusCreated, claimed, corsOrigin)
	}
}

func nearbyAccessIdentity(request *http.Request, loopback, tailnetIP bool) (tailnetIdentity, string, string, bool) {
	address, ok := privateRemoteIPv4(request.RemoteAddr)
	transport := "nearby"
	if tailnetIP {
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		address = tailscale.TailnetIPv4([]string{host})
		ok = address != ""
		transport = "tailnet-ip"
	}
	if loopback {
		address, ok = "127.0.0.1", true
	}
	return tailnetIdentity{Login: transport + ":" + address}, transport, address, ok
}

func (s *Server) completeAccessClaim(claim *pairingClaimResponse) {
	claim.MachineID = s.identity.ID
	claim.MachineName = s.identity.Name
	s.addMachineEndpoints(claim)
}

func (s *Server) addMachineEndpoints(claim *pairingClaimResponse) {
	lan := s.lan.state()
	if lan.URL != nil {
		claim.LANEndpoint = *lan.URL
	}
	remote := s.remote.state()
	claim.TailnetEndpoint = remote.Endpoint
	claim.TailnetIPEndpoint = remote.TailnetIPEndpoint
	claim.RelayEndpoint = s.relayMachineEndpoint()
}

// Access-request administration answers on two paths for the same resource.
//
//   - accessRequestsPath is canonical. It covers tailnet AND nearby/LAN
//     requests, which is why it is not named after tailnet, and it is what the
//     sessions CLI (`sessions machines`) calls and what the frontend tries
//     first.
//   - legacyTailnetAccessRequestsPath is the original name, still documented in
//     CONTRACT/http-api.md and still used by the frontend as its fallback for
//     daemons that predate the canonical path. It is DEPRECATED: it stays until
//     a release whose clients all speak the canonical path, and responses on it
//     say so with RFC 8594 Deprecation/Link headers so the situation is visible
//     to a client instead of buried in this file.
const (
	accessRequestsPath               = "/api/access/requests"
	legacyTailnetAccessRequestsPath  = "/api/tailnet/access/requests"
	accessRequestsDeprecationLinkFmt = `<%s>; rel="successor-version"`
)

func (s *Server) handleTailnetAccessAdminRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	collection := ""
	for _, candidate := range []string{accessRequestsPath, legacyTailnetAccessRequestsPath} {
		if request.URL.Path == candidate || strings.HasPrefix(request.URL.Path, candidate+"/") {
			collection = candidate
			break
		}
	}
	if collection == "" {
		return false
	}
	if collection == legacyTailnetAccessRequestsPath {
		response.Header().Set("Deprecation", "true")
		response.Header().Set("Link", fmt.Sprintf(accessRequestsDeprecationLinkFmt,
			accessRequestsPath+strings.TrimPrefix(request.URL.Path, collection)))
	}
	if !s.requireLocalPrincipal(response, request, corsOrigin, "Access request administration") {
		return true
	}
	if request.URL.Path == collection {
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"requests": s.tailnetAccess.list()}, corsOrigin)
		return true
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	requestID := strings.TrimPrefix(request.URL.Path, collection+"/")
	if requestID == "" || strings.Contains(requestID, "/") {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "access request not found"}, corsOrigin)
		return true
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	decided, err := s.tailnetAccess.decide(requestID, body.Decision)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTailnetAccessGone) {
			status = http.StatusNotFound
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	log.Printf("sessionsd: %s access request %s from %s", body.Decision, shortPairTicketID(decided.RequestID), decided.Name)
	s.sendJSON(response, http.StatusOK, decided, corsOrigin)
	return true
}

func requestHasJSONContentType(request *http.Request) bool {
	contentTypes := request.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	return err == nil && mediaType == "application/json"
}

func privateRemoteIPv4(remoteAddress string) (string, bool) {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		host = strings.Trim(remoteAddress, "[]")
	}
	ip := net.ParseIP(host).To4()
	if ip == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsMulticast() {
		return "", false
	}
	return ip.String(), true
}
