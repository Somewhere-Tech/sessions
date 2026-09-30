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

	// fleetDialTimeout bounds one dial to one saved address.
	fleetDialTimeout = 5 * time.Second

	// fleetPeerBudget bounds everything this host will spend deciding how to
	// reach one peer. A machine that is off, mid-install, or on another network
	// used to cost this once per saved address in turn, so a peer with three
	// routes held a request for three dial timeouts before anyone was told.
	fleetPeerBudget = 5 * time.Second

	// fleetNextRouteDelay is how long a higher-preference route gets to itself
	// before the next one starts in parallel. A reachable LAN peer answers well
	// inside this, so the ordinary case still makes exactly one connection and
	// the saved order still decides who wins; a dead route no longer costs the
	// next route its turn.
	fleetNextRouteDelay = 300 * time.Millisecond
)

type fleetSavedMachine struct {
	MachineID         string `json:"machine_id"`
	Name              string `json:"name"`
	Endpoint          string `json:"endpoint"`
	LANEndpoint       string `json:"lan_endpoint,omitempty"`
	TailnetEndpoint   string `json:"tailnet_endpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnet_ip_endpoint,omitempty"`
	RelayEndpoint     string `json:"relay_endpoint,omitempty"`
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
	RelayEndpoint     string `json:"relay_endpoint,omitempty"`
	Reachable         bool   `json:"reachable"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
}

var fleetRelayTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: fleetDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
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
		// Every saved route was unusable or unanswered. That is a statement
		// about the destination, not about this host, so it must not read as
		// this daemon having failed.
		body := map[string]any{"error": err.Error()}
		if errors.Is(err, errFleetEndpointUnusable) {
			body["reason"] = fleetEndpointUnusableReason
		}
		s.sendJSON(response, http.StatusBadGateway, body, corsOrigin)
		return true
	}

	log.Printf("sessionsd: fleet relay method=%s path=%s machine=%s device_id=%s", request.Method, remotePath, machine.MachineID, deviceID)
	s.fleetRelayProxy(fleetRelayRoute{
		target: target, selected: selected, remotePath: remotePath,
		credential: credential, machineID: machine.MachineID, deviceID: deviceID,
		corsOrigin: corsOrigin,
	}).ServeHTTP(response, request)
	return true
}

type fleetRelayRoute struct {
	target     *url.URL
	selected   fleetendpoint.Candidate
	remotePath string
	credential string
	machineID  string
	deviceID   string
	corsOrigin string
}

func (s *Server) fleetRelayProxy(route fleetRelayRoute) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     fleetRelayTransport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(route.target)
			proxyRequest.Out.URL.Path = route.remotePath
			proxyRequest.Out.URL.RawPath = ""
			query := proxyRequest.Out.URL.Query()
			query.Del("token")
			proxyRequest.Out.URL.RawQuery = query.Encode()
			proxyRequest.Out.Header.Del("Authorization")
			proxyRequest.Out.Header.Del("Proxy-Authorization")
			proxyRequest.Out.Header.Set("Authorization", "Bearer "+route.credential)
			// SetXForwarded first removes caller-supplied forwarding claims, then
			// records the phone as the peer. This also keeps a second scratch
			// daemon on loopback from mistaking the relay for ambient local trust.
			proxyRequest.SetXForwarded()
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
			log.Printf("sessionsd: fleet relay failed machine=%s device_id=%s: %v", route.machineID, route.deviceID, proxyErr)
			explained := fleetEndpointError(route.selected.Endpoint, proxyErr)
			body := map[string]any{"error": explained.Error()}
			if localnetwork.IsPossiblePermissionError(explained) {
				body["reason"] = localnetwork.Reason
			}
			s.sendJSON(writer, http.StatusBadGateway, body, route.corsOrigin)
		},
	}
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
		views[index] = fleetMachineListing(machine)
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

// fleetMachineListing publishes a saved machine's identity and only the
// addresses this host would be willing to dial. The registry holds whatever was
// saved -- a machine claimed from the account directory keeps the addresses that
// directory published -- so a row can carry userinfo, a query, or a fragment.
// An address that fails the dial rules is not shown either: it can never be used
// from here, and this response promises to carry no credential. Its neighbours
// on the same row, and every other machine, are unaffected.
func fleetMachineListing(machine fleetSavedMachine) fleetMachineView {
	publishable := func(endpoint, transport string) string {
		if endpoint == "" {
			return ""
		}
		candidate := fleetendpoint.Candidate{Endpoint: endpoint, Transport: transport}
		if validateFleetCandidate(machine.MachineID, candidate) != nil {
			return ""
		}
		return endpoint
	}
	return fleetMachineView{
		ID: machine.MachineID, Name: machine.Name,
		LANEndpoint:       publishable(machine.LANEndpoint, "lan"),
		TailnetEndpoint:   publishable(machine.TailnetEndpoint, "tailnet"),
		TailnetIPEndpoint: publishable(machine.TailnetIPEndpoint, "tailnet-ip"),
		RelayEndpoint:     publishable(machine.RelayEndpoint, "relay"),
	}
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
		// Selection returns the candidate it reached, or the only saved endpoint
		// without probing it; either way that is the address this row failed at.
		if err = probeFleetEndpoint(ctx, target, credential, machine.MachineID); err != nil {
			err = fleetEndpointError(selected.Endpoint, err)
		}
	}
	if err != nil {
		switch {
		case localnetwork.IsPossiblePermissionError(err):
			view.Reason, view.Message = localnetwork.Reason, err.Error()
		case errors.Is(err, errFleetEndpointUnusable):
			view.Reason, view.Message = fleetEndpointUnusableReason, err.Error()
		}
		return view
	}
	s.observeFleetTransport(selected)
	view.Endpoint, view.Transport, view.Reachable = selected.Endpoint, selected.Transport, true
	return view
}

// fleetEndpointError names the endpoint this attempt actually dialled. The
// machine's saved primary address is not evidence about a fallback route that
// failed, and attributing a tailnet failure to it invented LAN problems.
func fleetEndpointError(endpoint string, err error) error {
	return fmt.Errorf("reach %s: %w", endpoint, localnetwork.Explain(endpoint, err))
}

// observeFleetTransport records the only local-network fact this daemon can
// prove: macOS cannot be blocking local access while a LAN peer is answering.
// A tailnet or relay success says nothing about the LAN and marks nothing.
func (s *Server) observeFleetTransport(selected fleetendpoint.Candidate) {
	if selected.Transport == "lan" && localnetwork.IsLocalEndpoint(selected.Endpoint) {
		s.lan.markPermission("granted")
	}
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
	// Deciding how to reach one peer is bounded, and the saved routes overlap
	// rather than queue: a peer that is off cost one dial timeout per saved
	// address in turn, so a caller waited three of them to be told nothing.
	// awaitFleetRoute below decides which overlapping answer wins.
	budget, cancel := context.WithTimeout(ctx, fleetPeerBudget)
	defer cancel()
	results := s.probeFleetCandidates(budget, candidates, credential, machine.MachineID)
	winner, err := s.awaitFleetRoute(budget, candidates, results)
	if err != nil {
		return nil, fleetendpoint.Candidate{}, err
	}
	s.observeFleetTransport(winner.candidate)
	return winner.target, winner.candidate, nil
}

// awaitFleetRoute picks which saved route this request uses.
//
// The saved order is a preference, not a queue. A route that answers is taken
// unless a route preferred over it is still deciding, and a still-deciding
// preferred route gets one more head start's worth of time to win -- no more.
// A silent preferred route therefore costs a caller that head start rather than
// the whole peer budget, while a preferred route that is merely a little slower
// than its neighbour still wins. Returning cancels the budget, so nothing waits
// on the routes that lost and their probes end; a dial already in flight may
// still finish inside net/http and be discarded, which costs no caller anything
// but is not the same as the socket closing at that instant.
func (s *Server) awaitFleetRoute(
	ctx context.Context, candidates []fleetendpoint.Candidate, results <-chan fleetProbe,
) (fleetProbe, error) {
	reported := make([]bool, len(candidates))
	failures := make([]error, len(candidates))
	best := -1
	var winner fleetProbe
	var grace <-chan time.Time
	for outstanding := len(candidates); outstanding > 0 && preferredStillDeciding(reported, best); {
		select {
		case probe := <-results:
			outstanding--
			reported[probe.index] = true
			if probe.err != nil {
				failures[probe.index] = fleetEndpointError(probe.candidate.Endpoint, probe.err)
				if probe.index == 0 && localnetwork.IsPossiblePermissionError(failures[0]) {
					s.logLANFallbackOnce(candidates[1:])
				}
				continue
			}
			if best < 0 || probe.index < best {
				best, winner = probe.index, probe
			}
			if grace == nil && best > 0 {
				timer := time.NewTimer(fleetNextRouteDelay)
				defer timer.Stop()
				grace = timer.C
			}
		case <-grace:
			return winner, nil
		case <-ctx.Done():
			if best >= 0 {
				return winner, nil
			}
			return fleetProbe{}, fleetRoutesFailure(candidates, failures, ctx.Err())
		}
	}
	if best >= 0 {
		return winner, nil
	}
	return fleetProbe{}, fleetRoutesFailure(candidates, failures, nil)
}

// preferredStillDeciding reports whether any route preferred over the best
// answer so far could still overtake it. With no answer yet, every route can.
func preferredStillDeciding(reported []bool, best int) bool {
	if best == 0 {
		return false
	}
	limit := len(reported)
	if best > 0 {
		limit = best
	}
	for index := range limit {
		if !reported[index] {
			return true
		}
	}
	return false
}

// fleetRoutesFailure names every route this host tried and what each one said.
// One endpoint's failure is not the story when several were attempted, and the
// caller cannot tell which address to look at unless it is told all of them.
func fleetRoutesFailure(candidates []fleetendpoint.Candidate, failures []error, budgetErr error) error {
	attempted := make([]error, 0, len(candidates))
	for index, failure := range failures {
		if failure != nil {
			attempted = append(attempted, failure)
			continue
		}
		if budgetErr != nil {
			attempted = append(attempted, fleetEndpointError(candidates[index].Endpoint, budgetErr))
		}
	}
	if len(attempted) == 1 {
		return attempted[0]
	}
	return &fleetRoutesError{attempted: attempted}
}

// fleetRoutesError keeps every route's own failure inspectable -- errors.Is and
// errors.As still see each one -- while reading as one sentence.
type fleetRoutesError struct{ attempted []error }

func (e *fleetRoutesError) Error() string {
	parts := make([]string, 0, len(e.attempted))
	for _, failure := range e.attempted {
		parts = append(parts, failure.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *fleetRoutesError) Unwrap() []error { return e.attempted }

type fleetProbe struct {
	index     int
	candidate fleetendpoint.Candidate
	target    *url.URL
	err       error
}

// probeFleetCandidates starts every saved route, each after a short head start
// for the routes preferred over it. A reachable LAN peer answers inside that
// head start, so the ordinary request still makes one connection; a route that
// hangs no longer costs the next route its turn. Answers arrive on one channel
// in whatever order they finish, and awaitFleetRoute applies the saved order to
// them.
func (s *Server) probeFleetCandidates(
	ctx context.Context, candidates []fleetendpoint.Candidate, credential, machineID string,
) <-chan fleetProbe {
	results := make(chan fleetProbe, len(candidates))
	settled := make([]chan struct{}, len(candidates))
	for index := range candidates {
		settled[index] = make(chan struct{})
	}
	for index, candidate := range candidates {
		result, done := results, settled[index]
		var previous chan struct{}
		if index > 0 {
			previous = settled[index-1]
		}
		go func(index int, candidate fleetendpoint.Candidate) {
			defer close(done)
			if index > 0 {
				timer := time.NewTimer(fleetNextRouteDelay)
				defer timer.Stop()
				select {
				case <-previous:
					// The route ahead of this one is already finished, so there
					// is nothing left to give it a head start for.
				case <-timer.C:
				case <-ctx.Done():
					result <- fleetProbe{index: index, candidate: candidate, err: ctx.Err()}
					return
				}
			}
			target, parseErr := url.Parse(candidate.Endpoint)
			if parseErr != nil {
				result <- fleetProbe{index: index, candidate: candidate, err: parseErr}
				return
			}
			result <- fleetProbe{
				index: index, candidate: candidate, target: target,
				err: probeFleetEndpoint(ctx, target, credential, machineID),
			}
		}(index, candidate)
	}
	return results
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

// errFleetEndpointUnusable marks a saved machine whose addresses this host
// cannot dial. A machine claimed from the account directory is saved with the
// addresses that directory published, so a host can end up holding a row it has
// no transport for. That is one machine's problem; it is not evidence about the
// rest of the fleet and never a reason to hide it.
var errFleetEndpointUnusable = errors.New("no saved address this host can use")

const fleetEndpointUnusableReason = "saved-endpoint-unusable"

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
	machines := make([]fleetSavedMachine, 0, len(registry.Machines))
	for _, machine := range registry.Machines {
		// The id is how a machine is named in this API and in a relay path, so a
		// row without a usable one has no identity to show or route to and is
		// dropped. Everything else is judged per row when that row is used: one
		// entry this host cannot dial must not take the whole fleet with it.
		if !validFleetMachineID(machine.MachineID) {
			continue
		}
		machines = append(machines, machine)
	}
	return machines, nil
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
	lan, tailnet, tailnetIP, relayEndpoint := machine.LANEndpoint, machine.TailnetEndpoint, machine.TailnetIPEndpoint, machine.RelayEndpoint
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
	case "relay":
		if relayEndpoint == "" {
			relayEndpoint = machine.Endpoint
		}
	}
	candidates := fleetendpoint.OrderedWithRelay(lan, tailnet, tailnetIP, relayEndpoint)
	for _, candidate := range candidates {
		if err := validateFleetCandidate(machine.MachineID, candidate); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: saved machine %q has no endpoint", errFleetEndpointUnusable, machine.MachineID)
	}
	return candidates, nil
}

func validateFleetCandidate(machineID string, candidate fleetendpoint.Candidate) error {
	parsed, err := url.Parse(strings.TrimSpace(candidate.Endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(candidate.Transport != "relay" && parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("%w: saved machine %q has an unusable endpoint", errFleetEndpointUnusable, machineID)
	}
	valid := candidate.Transport == "lan" && parsed.Scheme == "http"
	valid = valid || candidate.Transport == "tailnet" && parsed.Scheme == "https" && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".ts.net")
	valid = valid || candidate.Transport == "tailnet-ip" && parsed.Scheme == "http" && tailscale.TailnetIPv4([]string{parsed.Hostname()}) != ""
	relayScheme := parsed.Scheme == "https" || parsed.Scheme == "http" && net.ParseIP(parsed.Hostname()) != nil && net.ParseIP(parsed.Hostname()).IsLoopback()
	valid = valid || candidate.Transport == "relay" && relayScheme && parsed.Path == "/m/"+url.PathEscape(machineID)
	if !valid {
		return fmt.Errorf("%w: saved machine %q has an address this host cannot use as a %s route",
			errFleetEndpointUnusable, machineID, candidate.Transport)
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
