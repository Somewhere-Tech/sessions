package main

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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/discovery"
	"github.com/somewhere-tech/sessions/runtime/internal/fleetaccount"
	"github.com/somewhere-tech/sessions/runtime/internal/fleetendpoint"
	"github.com/somewhere-tech/sessions/runtime/internal/localnetwork"
	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

const machineRegistryVersion = 1
const nativeAppMachineSource = "native-app"
const accountMachineSource = "account"

type savedMachine struct {
	Alias             string    `json:"alias"`
	MachineID         string    `json:"machine_id"`
	Name              string    `json:"name"`
	Endpoint          string    `json:"endpoint"`
	LANEndpoint       string    `json:"lan_endpoint,omitempty"`
	TailnetEndpoint   string    `json:"tailnet_endpoint,omitempty"`
	TailnetIPEndpoint string    `json:"tailnet_ip_endpoint,omitempty"`
	RelayEndpoint     string    `json:"relay_endpoint,omitempty"`
	Transport         string    `json:"transport"`
	DeviceID          string    `json:"device_id"`
	ConnectedAt       time.Time `json:"connected_at"`
	Source            string    `json:"source,omitempty"`
}

type machineRegistry struct {
	Version  int            `json:"version"`
	Machines []savedMachine `json:"machines"`
}

type nativeMachineSyncItem struct {
	Alias             string `json:"alias,omitempty"`
	MachineID         string `json:"machineId"`
	Name              string `json:"name"`
	Endpoint          string `json:"endpoint"`
	LANEndpoint       string `json:"lanEndpoint,omitempty"`
	TailnetEndpoint   string `json:"tailnetEndpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnetIpEndpoint,omitempty"`
	RelayEndpoint     string `json:"relayEndpoint,omitempty"`
	DeviceID          string `json:"deviceId,omitempty"`
	Token             string `json:"token"`
}

type nativeMachineSyncRequest struct {
	Machines []nativeMachineSyncItem `json:"machines"`
}

type preparedNativeMachine struct {
	Machine savedMachine
	Token   string
}

type accessRequest struct {
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

type accessRequestsResponse struct {
	Requests []accessRequest `json:"requests"`
}

type accessBootstrap struct {
	RequestID     string    `json:"request_id"`
	RequestSecret string    `json:"request_secret"`
	ExpiresAt     time.Time `json:"expires_at"`
	Status        string    `json:"status"`
}

type accessClaim struct {
	DeviceID          string `json:"device_id"`
	Token             string `json:"token"`
	Name              string `json:"name"`
	MachineID         string `json:"machine_id"`
	MachineName       string `json:"machine_name"`
	LANEndpoint       string `json:"lan_endpoint,omitempty"`
	TailnetEndpoint   string `json:"tailnet_endpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnet_ip_endpoint,omitempty"`
	RelayEndpoint     string `json:"relay_endpoint,omitempty"`
}

type nearbyHealth struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	SessionsLoaded int    `json:"sessionsLoaded"`
	System         struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	} `json:"system"`
}

type discoveredMachine struct {
	discovery.Candidate
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	SessionsLoaded int    `json:"sessions_loaded"`
	Reachable      bool   `json:"reachable"`
}

type fleetMachineView struct {
	Alias      string                    `json:"alias,omitempty"`
	MachineID  string                    `json:"machine_id,omitempty"`
	Name       string                    `json:"name"`
	Sources    []string                  `json:"sources"`
	Candidates []fleetendpoint.Candidate `json:"candidates"`
	InUse      *fleetendpoint.Candidate  `json:"in_use,omitempty"`
	Credential bool                      `json:"credential"`
	LastSeenAt string                    `json:"last_seen_at,omitempty"`
}

func (a *app) cmdMachines(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		if len(args) > 1 {
			return fail(1, "usage: sessions machines [list]")
		}
		return a.listSavedMachines()
	}
	switch args[0] {
	case "discover":
		return a.discoverMachines(args[1:])
	case "connect":
		return a.connectMachine(args[1:])
	case "forget":
		return a.forgetMachine(args[1:])
	case "sync-native":
		return a.syncNativeMachines(args[1:])
	default:
		// sync-native is dispatched above but was missing from this line, so
		// the one place a caller looks after a typo did not list it.
		return fail(1, "usage: sessions machines <discover|connect|list|forget|sync-native>")
	}
}

func (a *app) syncNativeMachines(args []string) error {
	if len(args) != 0 {
		return fail(1, "usage: sessions machines sync-native < JSON")
	}
	var request nativeMachineSyncRequest
	decoder := json.NewDecoder(io.LimitReader(a.stdin, 256*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fail(1, "read native machine registry: %s", err)
	}
	if len(request.Machines) > 100 {
		return fail(1, "native machine registry cannot contain more than 100 machines")
	}
	prepared, seen, err := a.prepareNativeMachines(request.Machines)
	if err != nil {
		return err
	}

	synced := make([]savedMachine, 0, len(prepared))
	for _, item := range prepared {
		record, err := saveMachine(a.home, item.Machine, item.Token)
		if err != nil {
			return fail(2, "save native machine %s: %s", item.Machine.Name, err)
		}
		synced = append(synced, record)
	}

	if err := a.removeStaleNativeMachines(seen); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Machines []savedMachine `json:"machines"`
		}{Machines: synced}, true)
	}
	_, err = fmt.Fprintf(a.stdout, "Synced %d native Sessions machine(s) for agent access.\n", len(synced))
	return err
}

func (a *app) prepareNativeMachines(items []nativeMachineSyncItem) ([]preparedNativeMachine, map[string]bool, error) {
	seen := make(map[string]bool, len(items))
	prepared := make([]preparedNativeMachine, 0, len(items))
	for _, item := range items {
		item.MachineID = strings.TrimSpace(item.MachineID)
		item.Name = strings.TrimSpace(item.Name)
		item.DeviceID = strings.TrimSpace(item.DeviceID)
		item.Token = strings.TrimSpace(item.Token)
		if err := validateMachineID(item.MachineID); err != nil {
			return nil, nil, fail(1, "native machine %q has an invalid stable id: %s", item.Name, err)
		}
		if seen[item.MachineID] {
			return nil, nil, fail(1, "native machine registry contains duplicate machine %s", item.MachineID)
		}
		if item.Token == "" || len(item.Token) > 512 || strings.ContainsAny(item.Token, "\r\n") {
			return nil, nil, fail(1, "native machine %s has an invalid device credential", item.Name)
		}
		seen[item.MachineID] = true
		endpoint, transport, err := validateMachineEndpoint(item.Endpoint)
		if err != nil {
			return nil, nil, fail(1, "native machine %s: %s", item.Name, err)
		}
		for _, candidate := range []struct {
			value *string
			kind  string
		}{
			{&item.LANEndpoint, "lan"}, {&item.TailnetEndpoint, "tailnet"}, {&item.TailnetIPEndpoint, "tailnet-ip"}, {&item.RelayEndpoint, "relay"},
		} {
			if *candidate.value == "" {
				continue
			}
			validated, _, err := validateMachineEndpointKind(*candidate.value, candidate.kind)
			if err != nil {
				return nil, nil, fail(1, "native machine %s: %s", item.Name, err)
			}
			*candidate.value = validated
		}
		prepared = append(prepared, preparedNativeMachine{Machine: savedMachine{
			Alias: item.Alias, MachineID: item.MachineID, Name: item.Name,
			Endpoint: endpoint, Transport: transport, DeviceID: item.DeviceID,
			LANEndpoint: item.LANEndpoint, TailnetEndpoint: item.TailnetEndpoint,
			TailnetIPEndpoint: item.TailnetIPEndpoint,
			RelayEndpoint:     item.RelayEndpoint,
			ConnectedAt:       a.now().UTC(), Source: nativeAppMachineSource,
		}, Token: item.Token})
	}
	return prepared, seen, nil
}

func (a *app) removeStaleNativeMachines(seen map[string]bool) error {
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return fail(2, "read saved machines: %s", err)
	}
	kept := registry.Machines[:0]
	removedTokenPaths := make([]string, 0)
	for _, machine := range registry.Machines {
		if machine.Source == nativeAppMachineSource && !seen[machine.MachineID] {
			// Drop the record either way; only remove a file whose path this
			// machine id is allowed to name.
			if tokenPath, err := machineTokenPathFor(a.home, machine.MachineID); err == nil {
				removedTokenPaths = append(removedTokenPaths, tokenPath)
			}
			continue
		}
		kept = append(kept, machine)
	}
	registry.Machines = kept
	if err := writeMachineRegistry(a.home, registry); err != nil {
		return fail(2, "finish native machine sync: %s", err)
	}
	for _, tokenPath := range removedTokenPaths {
		_ = os.Remove(tokenPath)
	}
	return nil
}

func (a *app) discoverMachines(args []string) error {
	timeoutRaw, hasTimeout := pluck(&args, "--timeout")
	if len(args) != 0 {
		return fail(1, "usage: sessions machines discover [--timeout D]")
	}
	timeout := 3 * time.Second
	var err error
	if hasTimeout {
		timeout, err = parseDuration(timeoutRaw, timeout)
		if err != nil {
			return err
		}
	}
	if !a.direct {
		return a.discoverMachinesViaDaemon(timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
	defer cancel()
	candidates, err := discovery.Browse(ctx, timeout)
	if err != nil {
		if a.localDaemonIsAdvertising() {
			return fail(2, "%s", localnetwork.Message)
		}
		return fail(2, "nearby discovery failed: %s", err)
	}
	machines := make([]discoveredMachine, 0, len(candidates))
	for _, candidate := range candidates {
		machine := discoveredMachine{Candidate: candidate}
		health, healthErr := fetchNearbyHealth(ctx, candidate.Endpoint)
		if healthErr != nil {
			continue
		}
		machine.Version = health.Version
		machine.OS = health.System.OS
		machine.Arch = health.System.Arch
		machine.SessionsLoaded = health.SessionsLoaded
		machine.Reachable = health.OK && health.Name == "sessionsd"
		if machine.Reachable {
			machines = append(machines, machine)
		}
	}
	return a.writeDiscoveredMachines(machines)
}

func (a *app) discoverMachinesViaDaemon(timeout time.Duration) error {
	var response struct {
		Machines []discoveredMachine `json:"machines"`
	}
	if err := a.getJSON("/api/lan/discover?timeout="+url.QueryEscape(timeout.String()), &response); err != nil {
		return err
	}
	return a.writeDiscoveredMachines(response.Machines)
}

func (a *app) writeDiscoveredMachines(machines []discoveredMachine) error {
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Machines []discoveredMachine `json:"machines"`
			Warning  string              `json:"warning"`
		}{
			Machines: machines,
			Warning:  "Nearby access uses unencrypted HTTP. Connect only on a private network you trust.",
		}, true)
	}
	if len(machines) == 0 {
		if a.localDaemonIsAdvertising() {
			return fail(2, "%s", localnetwork.Message)
		}
		_, err := fmt.Fprintln(a.stdout, "No nearby Sessions machines found. Make sure LAN access is enabled on the other machine.")
		return err
	}
	writer := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tENDPOINT\tSYSTEM\tSESSIONS"); err != nil {
		return err
	}
	for _, machine := range machines {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s/%s\t%d\n",
			machine.Name, machine.Endpoint, machine.OS, machine.Arch, machine.SessionsLoaded); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.stdout, "\nNearby access is unencrypted. Connect only on a private network you trust.")
	return err
}

func (a *app) connectMachine(args []string) error {
	alias, _ := pluck(&args, "--name")
	timeoutRaw, hasTimeout := pluck(&args, "--timeout")
	lanEndpoint, hasLAN := pluck(&args, "--lan")
	tailnetEndpoint, hasTailnet := pluck(&args, "--tailnet")
	tailnetIPEndpoint, hasTailnetIP := pluck(&args, "--tailnet-ip")
	relayEndpoint, hasRelay := pluck(&args, "--relay")
	if len(args) != 1 {
		return fail(1, "usage: sessions machines connect <endpoint-or-pairing-link> [--lan URL] [--tailnet URL] [--tailnet-ip URL] [--relay URL] [--name ALIAS] [--timeout D]")
	}
	if looksLikePairingLink(args[0]) {
		if hasLAN || hasTailnet || hasTailnetIP || hasRelay || hasTimeout {
			return fail(1, "a pairing link cannot be combined with endpoint or timeout flags")
		}
		return a.connectMachinePairingLink(args[0], alias)
	}
	endpoint, transport, err := validateMachineEndpoint(args[0])
	if err != nil {
		return fail(1, "%s", err)
	}
	timeout := 10 * time.Minute
	if hasTimeout {
		timeout, err = parseDuration(timeoutRaw, timeout)
		if err != nil {
			return err
		}
		if timeout > 10*time.Minute {
			return fail(1, "--timeout cannot exceed 10m")
		}
	}
	if err := assignConnectionEndpoint(endpoint, transport, &lanEndpoint, &tailnetEndpoint, &tailnetIPEndpoint, &relayEndpoint); err != nil {
		return fail(1, "%s", err)
	}
	if hasLAN {
		lanEndpoint, _, err = validateMachineEndpointKind(lanEndpoint, "lan")
	}
	if err == nil && hasTailnet {
		tailnetEndpoint, _, err = validateMachineEndpointKind(tailnetEndpoint, "tailnet")
	}
	if err == nil && hasTailnetIP {
		tailnetIPEndpoint, _, err = validateMachineEndpointKind(tailnetIPEndpoint, "tailnet-ip")
	}
	if err == nil && hasRelay {
		relayEndpoint, _, err = validateMachineEndpointKind(relayEndpoint, "relay")
	}
	if err != nil {
		return fail(1, "%s", err)
	}
	candidates := fleetendpoint.OrderedWithRelay(lanEndpoint, tailnetEndpoint, tailnetIPEndpoint, relayEndpoint)
	if !a.direct {
		return a.connectMachineViaDaemon(candidates, alias, timeout)
	}
	claim, used, err := a.connectMachineDirect(candidates, timeout)
	if err != nil {
		return err
	}
	return a.finishMachineConnect(used.Endpoint, used.Transport, alias, claim)
}

func looksLikePairingLink(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "sessions://") || strings.Contains(lower, "/pair/") || strings.Contains(lower, "#pair=")
}

func (a *app) connectMachinePairingLink(value, alias string) error {
	endpoints, ticket, err := fleetendpoint.ParsePairingLink(value)
	if err != nil {
		return fail(1, "%s", err)
	}
	candidates := make([]fleetendpoint.Candidate, 0, len(endpoints))
	for _, endpoint := range endpoints {
		validated, transport, validateErr := validateMachineEndpoint(endpoint)
		if validateErr != nil {
			return fail(1, "pairing link endpoint %q: %s", endpoint, validateErr)
		}
		if transport == "nearby" {
			transport = "lan"
		}
		candidates = append(candidates, fleetendpoint.Candidate{Endpoint: validated, Transport: transport})
	}
	if a.direct {
		claim, used, claimErr := claimMachinePairingTicket(candidates, ticket)
		if claimErr != nil {
			return claimErr
		}
		return a.finishMachineConnect(used.Endpoint, used.Transport, alias, claim)
	}
	deviceName, _ := os.Hostname()
	var response struct {
		Claim     accessClaim `json:"claim"`
		Endpoint  string      `json:"endpoint"`
		Transport string      `json:"transport"`
	}
	if err := a.postJSON("/api/lan/connect", map[string]string{
		"lan_endpoint":        endpointForTransport(candidates, "lan"),
		"tailnet_endpoint":    endpointForTransport(candidates, "tailnet"),
		"tailnet_ip_endpoint": endpointForTransport(candidates, "tailnet-ip"),
		"relay_endpoint":      endpointForTransport(candidates, "relay"),
		"ticket":              ticket, "name": deviceName,
	}, &response, 2); err != nil {
		return err
	}
	return a.finishMachineConnect(response.Endpoint, response.Transport, alias, response.Claim)
}

func claimMachinePairingTicket(candidates []fleetendpoint.Candidate, ticket string) (accessClaim, fleetendpoint.Candidate, error) {
	selected, err := selectDirectMachineEndpoint(candidates)
	if err != nil {
		return accessClaim{}, fleetendpoint.Candidate{}, err
	}
	deviceName, _ := os.Hostname()
	var claim accessClaim
	status, err := postBootstrapJSON(context.Background(), bootstrapHTTPClient(),
		selected.Endpoint+"/api/lan/access/claim", map[string]string{"ticket": ticket, "name": deviceName}, &claim)
	if err != nil {
		return accessClaim{}, fleetendpoint.Candidate{}, fail(2, "claim pairing ticket: %s", localnetwork.Explain(selected.Endpoint, err))
	}
	if status != http.StatusCreated {
		return accessClaim{}, fleetendpoint.Candidate{}, fail(2, "pairing ticket claim failed with HTTP %d", status)
	}
	if _, err := verifyMachineCredential(selected.Endpoint, claim.Token); err != nil {
		return accessClaim{}, fleetendpoint.Candidate{}, fail(2, "verify the issued machine credential: %s", err)
	}
	return claim, selected, nil
}

func (a *app) connectMachineDirect(candidates []fleetendpoint.Candidate, timeout time.Duration) (accessClaim, fleetendpoint.Candidate, error) {
	clientID, err := loadOrCreateClientID(a.home)
	if err != nil {
		return accessClaim{}, fleetendpoint.Candidate{}, fail(2, "prepare this device identity: %s", err)
	}
	deviceName, _ := os.Hostname()
	selected, err := selectDirectMachineEndpoint(candidates)
	if err != nil {
		return accessClaim{}, fleetendpoint.Candidate{}, err
	}
	claim, err := a.connectMachineAtEndpoint(selected, clientID, deviceName, timeout)
	return claim, selected, err
}

func selectDirectMachineEndpoint(candidates []fleetendpoint.Candidate) (fleetendpoint.Candidate, error) {
	var lastErr error
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		health, err := fetchNearbyHealth(ctx, candidate.Endpoint)
		cancel()
		if err == nil && health.OK && health.Name == "sessionsd" {
			return candidate, nil
		}
		if err == nil {
			err = errors.New("endpoint did not identify itself as sessionsd")
		}
		lastErr = fmt.Errorf("reach %s: %w", candidate.Endpoint, err)
	}
	return fleetendpoint.Candidate{}, lastErr
}

func (a *app) connectMachineAtEndpoint(candidate fleetendpoint.Candidate, clientID, deviceName string, timeout time.Duration) (accessClaim, error) {
	endpoint, transport := candidate.Endpoint, candidate.Transport
	requestPath, claimPath := accessPaths(transport)
	client := bootstrapHTTPClient()
	var created accessBootstrap
	statusCode, err := postBootstrapJSON(
		context.Background(), client, endpoint+requestPath,
		map[string]string{"client_id": clientID, "name": deviceName}, &created,
	)
	if err != nil {
		return accessClaim{}, fail(2, "request access from %s: %s", endpoint, localnetwork.Explain(endpoint, err))
	}
	if statusCode != http.StatusAccepted || created.RequestID == "" || created.RequestSecret == "" {
		return accessClaim{}, fail(2, "access request failed with HTTP %d", statusCode)
	}
	if !a.wantJSON {
		fmt.Fprintf(a.stdout, "Access requested from %s. Accept it on the other machine; waiting up to %s…\n", endpoint, timeout.Round(time.Second))
	}
	deadline := a.now().Add(timeout)
	var claim accessClaim
	for {
		if !a.now().Before(deadline) {
			return accessClaim{}, fail(2, "access request expired while waiting for approval; run the command again")
		}
		statusCode, err = postBootstrapJSON(
			context.Background(), client, endpoint+claimPath,
			map[string]string{"request_id": created.RequestID, "request_secret": created.RequestSecret}, &claim,
		)
		switch statusCode {
		case http.StatusCreated:
			if err != nil {
				return accessClaim{}, fail(2, "claim approved access: %s", localnetwork.Explain(endpoint, err))
			}
			goto connected
		case http.StatusAccepted:
			a.sleep(2 * time.Second)
			continue
		case http.StatusForbidden:
			return accessClaim{}, fail(2, "the other machine denied this access request")
		case http.StatusGone:
			return accessClaim{}, fail(2, "the access request expired; run the command again")
		default:
			if err != nil {
				return accessClaim{}, fail(2, "check access request: %s", localnetwork.Explain(endpoint, err))
			}
			return accessClaim{}, fail(2, "access claim failed with HTTP %d", statusCode)
		}
	}

connected:
	health, err := verifyMachineCredential(endpoint, claim.Token)
	if err != nil {
		return accessClaim{}, fail(2, "verify the issued machine credential: %s", localnetwork.Explain(endpoint, err))
	}
	if health.Name != "sessionsd" {
		return accessClaim{}, fail(2, "the approved endpoint is not sessionsd")
	}
	return claim, nil
}

func (a *app) connectMachineViaDaemon(candidates []fleetendpoint.Candidate, alias string, timeout time.Duration) error {
	clientID, err := loadOrCreateClientID(a.home)
	if err != nil {
		return fail(2, "prepare this device identity: %s", err)
	}
	deviceName, _ := os.Hostname()
	var response struct {
		Claim     accessClaim `json:"claim"`
		Endpoint  string      `json:"endpoint"`
		Transport string      `json:"transport"`
	}
	if !a.wantJSON {
		fmt.Fprintf(a.stdout, "Requesting access from %s. Accept it on the other machine; waiting up to %s…\n", candidates[0].Endpoint, timeout.Round(time.Second))
	}
	if err := a.postJSON("/api/lan/connect", map[string]string{
		"lan_endpoint":        endpointForTransport(candidates, "lan"),
		"tailnet_endpoint":    endpointForTransport(candidates, "tailnet"),
		"tailnet_ip_endpoint": endpointForTransport(candidates, "tailnet-ip"),
		"client_id":           clientID, "name": deviceName, "timeout": timeout.String(),
	}, &response, 2); err != nil {
		return err
	}
	used := fleetendpoint.Candidate{Endpoint: response.Endpoint, Transport: response.Transport}
	return a.finishMachineConnect(used.Endpoint, used.Transport, alias, response.Claim)
}

func assignConnectionEndpoint(endpoint, transport string, lan, tailnet, tailnetIP, relayEndpoint *string) error {
	switch transport {
	case "nearby":
		if *lan == "" {
			*lan = endpoint
		}
	case "tailnet":
		if *tailnet == "" {
			*tailnet = endpoint
		}
	case "tailnet-ip":
		if *tailnetIP == "" {
			*tailnetIP = endpoint
		}
	case "relay":
		if *relayEndpoint == "" {
			*relayEndpoint = endpoint
		}
	default:
		return fmt.Errorf("unknown machine transport %q", transport)
	}
	return nil
}

func endpointForTransport(candidates []fleetendpoint.Candidate, transport string) string {
	for _, candidate := range candidates {
		if candidate.Transport == transport {
			return candidate.Endpoint
		}
	}
	return ""
}

func validateMachineEndpointKind(raw, kind string) (string, string, error) {
	endpoint, transport, err := validateMachineEndpoint(raw)
	if err != nil {
		return "", "", err
	}
	wanted := kind
	if wanted == "lan" {
		wanted = "nearby"
	}
	if transport != wanted {
		return "", "", fmt.Errorf("%s endpoint has transport %s", kind, transport)
	}
	return endpoint, transport, nil
}

func (a *app) finishMachineConnect(endpoint, transport, alias string, claim accessClaim) error {
	if claim.Token == "" || claim.MachineID == "" || claim.DeviceID == "" {
		return fail(2, "the other machine returned an incomplete credential")
	}
	if err := validateMachineID(claim.MachineID); err != nil {
		return fail(2, "the other machine sent an unusable stable id, so no credential was saved: %s", err)
	}
	if claim.LANEndpoint == "" && transport == "lan" {
		claim.LANEndpoint = endpoint
	}
	if claim.TailnetEndpoint == "" && transport == "tailnet" {
		claim.TailnetEndpoint = endpoint
	}
	if claim.TailnetIPEndpoint == "" && transport == "tailnet-ip" {
		claim.TailnetIPEndpoint = endpoint
	}
	if claim.RelayEndpoint == "" && transport == "relay" {
		claim.RelayEndpoint = endpoint
	}
	record, err := saveMachine(a.home, savedMachine{
		Alias: alias, MachineID: claim.MachineID, Name: claim.MachineName,
		Endpoint: endpoint, Transport: transport, DeviceID: claim.DeviceID,
		ConnectedAt: a.now().UTC(),
		LANEndpoint: claim.LANEndpoint, TailnetEndpoint: claim.TailnetEndpoint,
		TailnetIPEndpoint: claim.TailnetIPEndpoint,
		RelayEndpoint:     claim.RelayEndpoint,
	}, claim.Token)
	if err != nil {
		return fail(2, "save machine credential: %s", err)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Machine savedMachine `json:"machine"`
			Use     string       `json:"use"`
		}{record, "sessions --machine " + record.Alias + " ls"}, true)
	}
	_, err = fmt.Fprintf(a.stdout, "Connected to %s as %q. Agents can use it with:\n  sessions --machine %s ls\n", record.Name, record.Alias, record.Alias)
	return err
}

func (a *app) localDaemonIsAdvertising() bool {
	var state struct {
		Bonjour struct {
			Advertised bool `json:"advertised"`
		} `json:"bonjour"`
	}
	return a.getJSON("/api/lan", &state) == nil && state.Bonjour.Advertised
}

func (a *app) listSavedMachines() error {
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return fail(2, "read saved machines: %s", err)
	}
	nearby, account, warnings := a.machineDirectorySources()
	registry, claimWarnings := a.claimAccountMachines(registry, account)
	warnings = append(warnings, claimWarnings...)
	machines := mergeMachineSources(registry.Machines, nearby, account)
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Machines []fleetMachineView `json:"machines"`
			Warnings []string           `json:"warnings,omitempty"`
		}{machines, warnings}, true)
	}
	if len(machines) == 0 {
		_, err := fmt.Fprintln(a.stdout, "No Sessions machines found. Sign in to Somewhere, scan a pairing code, or enable LAN discovery on another machine.")
		return err
	}
	writer := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ALIAS\tNAME\tSOURCES\tCANDIDATES\tIN USE"); err != nil {
		return err
	}
	for _, machine := range machines {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			machine.Alias, machine.Name, strings.Join(machine.Sources, ","),
			formatMachineCandidates(machine.Candidates), formatMachineInUse(machine.InUse)); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintln(a.stderr, "warning:", warning)
	}
	return nil
}

func (a *app) machineDirectorySources() ([]discoveredMachine, []fleetaccount.Machine, []string) {
	warnings := make([]string, 0, 2)
	nearby := []discoveredMachine{}
	if a.direct {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		candidates, err := discovery.Browse(ctx, time.Second)
		if err != nil {
			warnings = append(warnings, "Bonjour discovery: "+err.Error())
		} else {
			for _, candidate := range candidates {
				nearby = append(nearby, discoveredMachine{Candidate: candidate, Reachable: true})
			}
		}
	} else {
		var response struct {
			Machines []discoveredMachine `json:"machines"`
		}
		if err := a.getJSON("/api/lan/discover?timeout=1s", &response); err != nil {
			warnings = append(warnings, "Bonjour discovery: "+err.Error())
		} else {
			nearby = response.Machines
		}
	}
	account := []fleetaccount.Machine{}
	if !a.direct {
		var response struct {
			Machines []fleetaccount.Machine `json:"machines"`
			SignedIn bool                   `json:"signed_in"`
		}
		if err := a.getJSON("/api/account/machines", &response); err != nil {
			warnings = append(warnings, "account directory: "+err.Error())
		} else if response.SignedIn {
			account = response.Machines
		}
	}
	return nearby, account, warnings
}

func (a *app) claimAccountMachines(registry machineRegistry, machines []fleetaccount.Machine) (machineRegistry, []string) {
	if a.direct || len(machines) == 0 {
		return registry, nil
	}
	var local struct {
		MachineID string `json:"machine_id"`
	}
	_ = a.getJSON("/api/machine", &local)
	saved := make(map[string]bool, len(registry.Machines))
	for _, machine := range registry.Machines {
		saved[machine.MachineID] = true
	}
	warnings := []string{}
	for _, machine := range machines {
		if machine.ID == "" || machine.ID == local.MachineID || saved[machine.ID] ||
			len(fleetendpoint.OrderedWithRelay(machine.EndpointsJSON.LAN, machine.EndpointsJSON.Tailnet, machine.EndpointsJSON.TailnetIP, machine.EndpointsJSON.Relay)) == 0 {
			continue
		}
		var response struct {
			Claim     accessClaim `json:"claim"`
			Endpoint  string      `json:"endpoint"`
			Transport string      `json:"transport"`
		}
		if err := a.postJSON("/api/account/machines/claim", map[string]string{"machine_id": machine.ID}, &response, 2); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s account credential: %v", machine.Name, err))
			continue
		}
		record, err := saveMachine(a.home, savedMachine{
			MachineID: machine.ID, Name: machine.Name, Endpoint: response.Endpoint,
			Transport: response.Transport, DeviceID: response.Claim.DeviceID,
			ConnectedAt: a.now().UTC(), Source: accountMachineSource,
			LANEndpoint: machine.EndpointsJSON.LAN, TailnetEndpoint: machine.EndpointsJSON.Tailnet,
			TailnetIPEndpoint: machine.EndpointsJSON.TailnetIP,
			RelayEndpoint:     machine.EndpointsJSON.Relay,
		}, response.Claim.Token)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("save %s account credential: %v", machine.Name, err))
			continue
		}
		registry.Machines = append(registry.Machines, record)
		saved[machine.ID] = true
	}
	return registry, warnings
}

func mergeMachineSources(saved []savedMachine, nearby []discoveredMachine, account []fleetaccount.Machine) []fleetMachineView {
	views := make([]fleetMachineView, 0, len(saved)+len(nearby)+len(account))
	for _, machine := range saved {
		candidates := fleetendpoint.OrderedWithRelay(machine.LANEndpoint, machine.TailnetEndpoint, machine.TailnetIPEndpoint, machine.RelayEndpoint)
		if machine.Endpoint != "" && endpointForTransport(candidates, machine.Transport) == "" {
			candidates = append(candidates, fleetendpoint.Candidate{Endpoint: machine.Endpoint, Transport: listedMachineTransport(machine.Transport)})
		}
		inUse := fleetendpoint.Candidate{Endpoint: machine.Endpoint, Transport: listedMachineTransport(machine.Transport)}
		views = append(views, fleetMachineView{
			Alias: machine.Alias, MachineID: machine.MachineID, Name: machine.Name,
			Sources: []string{"saved"}, Candidates: candidates, InUse: &inUse, Credential: true,
		})
	}
	for _, machine := range account {
		index := machineViewIndex(views, machine.ID, accountMachineCandidates(machine))
		if index < 0 {
			views = append(views, fleetMachineView{MachineID: machine.ID, Name: machine.Name})
			index = len(views) - 1
		}
		views[index].Sources = appendMachineSource(views[index].Sources, "account")
		views[index].Candidates = mergeMachineCandidates(views[index].Candidates, accountMachineCandidates(machine))
		views[index].LastSeenAt = machine.LastSeenAt
		if views[index].Name == "" {
			views[index].Name = machine.Name
		}
	}
	for _, machine := range nearby {
		candidates := fleetendpoint.Ordered(machine.LANEndpoint, machine.TailnetEndpoint, machine.TailnetIPEndpoint)
		index := machineViewIndex(views, "", candidates)
		if index < 0 {
			views = append(views, fleetMachineView{Name: machine.Name})
			index = len(views) - 1
		}
		views[index].Sources = appendMachineSource(views[index].Sources, "bonjour")
		views[index].Candidates = mergeMachineCandidates(views[index].Candidates, candidates)
		if views[index].Name == "" {
			views[index].Name = machine.Name
		}
	}
	for index := range views {
		sort.Strings(views[index].Sources)
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name)
	})
	return views
}

func accountMachineCandidates(machine fleetaccount.Machine) []fleetendpoint.Candidate {
	result := fleetendpoint.Ordered(machine.EndpointsJSON.LAN, machine.EndpointsJSON.Tailnet, machine.EndpointsJSON.TailnetIP)
	if relay := strings.TrimSuffix(strings.TrimSpace(machine.EndpointsJSON.Relay), "/"); relay != "" {
		result = append(result, fleetendpoint.Candidate{Endpoint: relay, Transport: "relay"})
	}
	return result
}

func machineViewIndex(views []fleetMachineView, machineID string, candidates []fleetendpoint.Candidate) int {
	for index, view := range views {
		if machineID != "" && view.MachineID == machineID {
			return index
		}
		for _, existing := range view.Candidates {
			for _, candidate := range candidates {
				if existing.Endpoint == candidate.Endpoint {
					return index
				}
			}
		}
	}
	return -1
}

func mergeMachineCandidates(current, added []fleetendpoint.Candidate) []fleetendpoint.Candidate {
	for _, candidate := range added {
		found := false
		for _, existing := range current {
			found = found || existing.Endpoint == candidate.Endpoint
		}
		if !found {
			current = append(current, candidate)
		}
	}
	return current
}

func appendMachineSource(sources []string, source string) []string {
	for _, existing := range sources {
		if existing == source {
			return sources
		}
	}
	return append(sources, source)
}

func listedMachineTransport(value string) string {
	if value == "nearby" {
		return "lan"
	}
	return value
}

func formatMachineCandidates(candidates []fleetendpoint.Candidate) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, candidate.Transport+"="+candidate.Endpoint)
	}
	return strings.Join(parts, ", ")
}

func formatMachineInUse(candidate *fleetendpoint.Candidate) string {
	if candidate == nil {
		return "—"
	}
	return candidate.Transport + "=" + candidate.Endpoint
}

func (a *app) forgetMachine(args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fail(1, "usage: sessions machines forget <alias-or-id>")
	}
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return fail(2, "read saved machines: %s", err)
	}
	index, err := findMachine(registry.Machines, args[0])
	if err != nil {
		return err
	}
	forgotten := registry.Machines[index]
	registry.Machines = append(registry.Machines[:index], registry.Machines[index+1:]...)
	if err := writeMachineRegistry(a.home, registry); err != nil {
		return fail(2, "update saved machines: %s", err)
	}
	// Forget always drops the registry record. It removes a credential file only
	// when the saved id is one Sessions could legitimately have written.
	if tokenPath, pathErr := machineTokenPathFor(a.home, forgotten.MachineID); pathErr == nil {
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(2, "remove saved credential: %s", err)
		}
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Forgotten savedMachine `json:"forgotten"`
			Warning   string       `json:"warning"`
		}{forgotten, "This removes the local credential only. Revoke the paired device on the host to invalidate it."}, true)
	}
	_, err = fmt.Fprintf(a.stdout, "Forgot %s locally. Run `sessions devices revoke %s` on the host to invalidate its credential.\n", forgotten.Name, forgotten.DeviceID)
	return err
}

func (a *app) cmdAccess(args []string) error {
	if len(args) == 1 && args[0] == "requests" {
		var response accessRequestsResponse
		if err := a.getJSON("/api/access/requests", &response); err != nil {
			return err
		}
		if a.wantJSON {
			return writeJSON(a.stdout, response, true)
		}
		if len(response.Requests) == 0 {
			_, err := fmt.Fprintln(a.stdout, "No pending access requests.")
			return err
		}
		writer := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "ID\tNAME\tTRANSPORT\tIDENTITY\tEXPIRES"); err != nil {
			return err
		}
		for _, request := range response.Requests {
			identity := request.Login
			if request.UserName != "" {
				identity = request.UserName + " · " + request.Login
			}
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
				prefixString(request.RequestID, 8), request.Name, request.Transport, identity, request.ExpiresAt.Local().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		return writer.Flush()
	}
	if len(args) == 2 && (args[0] == "accept" || args[0] == "deny") {
		var decided accessRequest
		if err := a.postJSON(
			"/api/access/requests/"+url.PathEscape(args[1]),
			map[string]string{"decision": args[0]}, &decided, 2,
		); err != nil {
			return err
		}
		if a.wantJSON {
			return writeJSON(a.stdout, decided, true)
		}
		verb := "Accepted"
		if args[0] == "deny" {
			verb = "Denied"
		}
		_, err := fmt.Fprintf(a.stdout, "%s access for %s (%s).\n", verb, decided.Name, decided.Transport)
		return err
	}
	return fail(1, "usage: sessions access <requests|accept ID|deny ID>")
}

func validateMachineEndpoint(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("endpoint must be an origin such as http://192.168.1.20:8787 or https://machine.tailnet.ts.net")
	}
	if strings.HasPrefix(parsed.Path, "/m/") && parsed.Path != "/m/" {
		ip := net.ParseIP(parsed.Hostname())
		loopback := strings.EqualFold(parsed.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
		machineID := strings.TrimPrefix(strings.TrimSuffix(parsed.Path, "/"), "/m/")
		if (parsed.Scheme == "https" || parsed.Scheme == "http" && loopback) && validateMachineID(machineID) == nil {
			return strings.TrimSuffix(parsed.String(), "/"), "relay", nil
		}
		return "", "", fmt.Errorf("relay endpoints require HTTPS (or loopback HTTP for development)")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", "", fmt.Errorf("machine endpoint must be an origin or relay /m/<machine_id> URL")
	}
	parsed.Path = ""
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname()).To4()
		port, portErr := strconv.Atoi(parsed.Port())
		if ip != nil && tailscale.TailnetIPv4([]string{ip.String()}) != "" && portErr == nil && port >= 1024 && port <= 65535 {
			return strings.TrimSuffix(parsed.String(), "/"), "tailnet-ip", nil
		}
		if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) || portErr != nil || port < 1024 || port > 65535 {
			return "", "", fmt.Errorf("nearby HTTP endpoints require a private or loopback IPv4 literal and explicit port")
		}
		return strings.TrimSuffix(parsed.String(), "/"), "nearby", nil
	}
	if parsed.Scheme == "https" {
		hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if !strings.HasSuffix(hostname, ".ts.net") {
			return "", "", fmt.Errorf("HTTPS machine endpoints must be Tailscale Serve names ending in .ts.net")
		}
		return strings.TrimSuffix(parsed.String(), "/"), "tailnet", nil
	}
	return "", "", fmt.Errorf("endpoint scheme must be http for a trusted LAN or https for Tailscale")
}

func accessPaths(transport string) (string, string) {
	if transport == "nearby" || transport == "lan" || transport == "tailnet-ip" {
		return "/api/lan/access/request", "/api/lan/access/claim"
	}
	return "/api/tailnet/access/request", "/api/tailnet/access/claim"
}

func fetchNearbyHealth(ctx context.Context, endpoint string) (nearbyHealth, error) {
	var health nearbyHealth
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/health", nil)
	if err != nil {
		return health, err
	}
	response, err := bootstrapHTTPClient().Do(request)
	if err != nil {
		return health, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return health, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health)
	return health, err
}

func postBootstrapJSON(ctx context.Context, client *http.Client, target string, body, output any) (int, error) {
	encoded, err := compactJSON(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
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
		if err := json.Unmarshal(payload, output); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func bootstrapHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:   8 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func verifyMachineCredential(endpoint, token string) (nearbyHealth, error) {
	var health nearbyHealth
	request, err := http.NewRequest(http.MethodGet, endpoint+"/api/sessions", nil)
	if err != nil {
		return health, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := bootstrapHTTPClient().Do(request)
	if err != nil {
		return health, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return health, fmt.Errorf("credential verification returned HTTP %d", response.StatusCode)
	}
	return fetchNearbyHealth(context.Background(), endpoint)
}

// machineStateRoot is the platform user state root that owns saved machines.
// Rebuilding the Unix layout here made `sessions --machine NAME` look at a
// nonexistent %USERPROFILE%\.local\state\sessions on Windows, so a machine
// registered on that host could never be reached again from its own CLI.
func machineStateRoot(home string) string {
	return sessionstate.UserStateRootFor(home)
}

func machineRegistryPath(home string) string {
	return filepath.Join(machineStateRoot(home), "clients.json")
}

// maxMachineIDLength bounds a peer-supplied stable id. Real ids are UUIDs or
// short stable names; anything longer is a malformed or hostile registration.
const maxMachineIDLength = 128

// validateMachineID rejects any peer-supplied stable id that could steer a
// saved credential outside the Sessions client directory. The id becomes a
// filename in savedMachineTokenPath, and forget/sync-native later remove that
// same computed path, so a separator, a parent reference, or a control
// character here is a filesystem escape rather than a naming preference. Alias
// input is already normalized by sanitizeMachineAlias; this holds the id to the
// same standard.
func validateMachineID(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("machine id is empty; the other machine must send a stable non-empty id")
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("machine id %q has leading or trailing whitespace; expected a plain id such as a UUID", value)
	}
	if len(value) > maxMachineIDLength {
		return fmt.Errorf("machine id is %d characters; keep it within %d characters such as a UUID", len(value), maxMachineIDLength)
	}
	if value == "." || value == ".." || strings.Contains(value, "..") {
		return fmt.Errorf("machine id %q contains a parent-directory reference; expected a plain id such as a UUID", value)
	}
	if strings.HasPrefix(value, ".") {
		return fmt.Errorf("machine id %q starts with a dot; expected a plain id such as a UUID", value)
	}
	for _, character := range value {
		switch {
		case character == '/' || character == '\\':
			return fmt.Errorf("machine id %q contains a path separator; expected a plain id such as a UUID", value)
		case character < 0x20 || character == 0x7f:
			return fmt.Errorf("machine id contains a control character; expected a plain id such as a UUID")
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-' || character == '_' || character == '.':
		default:
			return fmt.Errorf("machine id %q contains %q; use only letters, digits, dot, dash, and underscore", value, string(character))
		}
	}
	return nil
}

func savedMachineTokenPath(home, machineID string) string {
	return filepath.Join(machineStateRoot(home), "clients", machineID+".token")
}

// machineTokenPathFor is the only safe way to compute a credential path: it
// refuses to build a path for an id that never should have been saved, so a
// poisoned registry cannot make Sessions write or delete outside its own
// client directory.
func machineTokenPathFor(home, machineID string) (string, error) {
	if err := validateMachineID(machineID); err != nil {
		return "", err
	}
	return savedMachineTokenPath(home, machineID), nil
}

func clientIDPath(home string) string {
	return filepath.Join(machineStateRoot(home), "client-id")
}

func loadOrCreateClientID(home string) (string, error) {
	path := clientIDPath(home)
	if encoded, err := os.ReadFile(path); err == nil {
		value := strings.TrimSpace(string(encoded))
		if value != "" {
			return value, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	value, err := randomUUID()
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(path, []byte(value+"\n")); err != nil {
		return "", err
	}
	return value, nil
}

func loadSavedMachine(home, reference string) (savedMachine, error) {
	registry, err := readMachineRegistry(home)
	if err != nil {
		return savedMachine{}, fail(2, "read saved machines: %s", err)
	}
	index, err := findMachine(registry.Machines, reference)
	if err != nil {
		return savedMachine{}, err
	}
	machine := registry.Machines[index]
	tokenPath, err := machineTokenPathFor(home, machine.MachineID)
	if err != nil {
		return savedMachine{}, fail(2, "saved machine %q has an unusable stable id: %s; run `sessions machines forget %s` and reconnect it", machine.Alias, err, machine.Alias)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		return savedMachine{}, fail(2, "saved credential for %q is missing; forget and reconnect this machine", machine.Alias)
	}
	return machine, nil
}

func findMachine(machines []savedMachine, reference string) (int, error) {
	reference = strings.TrimSpace(reference)
	for index, machine := range machines {
		if strings.EqualFold(machine.Alias, reference) || machine.MachineID == reference {
			return index, nil
		}
	}
	matches := make([]int, 0, 1)
	for index, machine := range machines {
		if strings.HasPrefix(machine.MachineID, reference) {
			matches = append(matches, index)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return -1, fail(1, "machine reference %q is ambiguous", reference)
	}
	return -1, fail(1, "unknown machine %q; run `sessions machines` to list saved machines", reference)
}

func readMachineRegistry(home string) (machineRegistry, error) {
	path := machineRegistryPath(home)
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return machineRegistry{Version: machineRegistryVersion, Machines: []savedMachine{}}, nil
	}
	if err != nil {
		return machineRegistry{}, err
	}
	var registry machineRegistry
	if err := json.Unmarshal(encoded, &registry); err != nil {
		return machineRegistry{}, err
	}
	if registry.Version != machineRegistryVersion {
		return machineRegistry{}, fmt.Errorf("unsupported saved-machine registry version %d", registry.Version)
	}
	if registry.Machines == nil {
		registry.Machines = []savedMachine{}
	}
	for index := range registry.Machines {
		normalizeSavedMachineEndpoints(&registry.Machines[index])
	}
	return registry, nil
}

func normalizeSavedMachineEndpoints(machine *savedMachine) {
	switch machine.Transport {
	case "nearby", "lan":
		if machine.LANEndpoint == "" {
			machine.LANEndpoint = machine.Endpoint
		}
	case "tailnet":
		if machine.TailnetEndpoint == "" {
			machine.TailnetEndpoint = machine.Endpoint
		}
	case "tailnet-ip":
		if machine.TailnetIPEndpoint == "" {
			machine.TailnetIPEndpoint = machine.Endpoint
		}
	case "relay":
		if machine.RelayEndpoint == "" {
			machine.RelayEndpoint = machine.Endpoint
		}
	}
}

func saveMachine(home string, machine savedMachine, token string) (savedMachine, error) {
	if machine.MachineID == "" || token == "" {
		return savedMachine{}, errors.New("machine id and token are required")
	}
	if err := validateMachineID(machine.MachineID); err != nil {
		return savedMachine{}, err
	}
	normalizeSavedMachineEndpoints(&machine)
	registry, err := readMachineRegistry(home)
	if err != nil {
		return savedMachine{}, err
	}
	if strings.TrimSpace(machine.Alias) == "" {
		for _, existing := range registry.Machines {
			if existing.MachineID == machine.MachineID {
				machine.Alias = existing.Alias
				break
			}
		}
		if machine.Alias == "" {
			machine.Alias = uniqueMachineAlias(registry.Machines, machine.Name)
		}
	} else {
		machine.Alias = sanitizeMachineAlias(machine.Alias)
		if machine.Alias == "" {
			return savedMachine{}, errors.New("machine alias must contain a letter or number")
		}
		for _, existing := range registry.Machines {
			if strings.EqualFold(existing.Alias, machine.Alias) && existing.MachineID != machine.MachineID {
				return savedMachine{}, fmt.Errorf("machine alias %q is already in use", machine.Alias)
			}
		}
	}
	replaced := false
	for index := range registry.Machines {
		if registry.Machines[index].MachineID == machine.MachineID {
			// A machine paired explicitly through the CLI remains CLI-owned even
			// when the native app refreshes its endpoint and credential. Removing
			// it from the app must not silently forget an independently approved
			// CLI connection.
			if registry.Machines[index].Source == "" && machine.Source == nativeAppMachineSource {
				machine.Source = ""
			}
			if machine.DeviceID == "" {
				machine.DeviceID = registry.Machines[index].DeviceID
			}
			registry.Machines[index] = machine
			replaced = true
			break
		}
	}
	if !replaced {
		registry.Machines = append(registry.Machines, machine)
	}
	sort.Slice(registry.Machines, func(i, j int) bool {
		return registry.Machines[i].Alias < registry.Machines[j].Alias
	})
	tokenPath, err := machineTokenPathFor(home, machine.MachineID)
	if err != nil {
		return savedMachine{}, err
	}
	previousToken, previousTokenErr := tokenstore.ReadSecret(tokenPath)
	if previousTokenErr != nil {
		return savedMachine{}, previousTokenErr
	}
	if err := tokenstore.WriteSecret(tokenPath, token); err != nil {
		return savedMachine{}, err
	}
	if err := writeMachineRegistry(home, registry); err != nil {
		if previousToken != "" {
			_ = tokenstore.WriteSecret(tokenPath, previousToken)
		} else {
			_ = os.Remove(tokenPath)
		}
		return savedMachine{}, err
	}
	return machine, nil
}

func writeMachineRegistry(home string, registry machineRegistry) error {
	registry.Version = machineRegistryVersion
	encoded, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(machineRegistryPath(home), append(encoded, '\n'))
}

func writePrivateFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".sessions-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func sanitizeMachineAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var cleaned strings.Builder
	dash := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			cleaned.WriteRune(character)
			dash = false
		} else if cleaned.Len() > 0 && !dash {
			cleaned.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(cleaned.String(), "-")
}

func uniqueMachineAlias(machines []savedMachine, name string) string {
	base := sanitizeMachineAlias(name)
	if base == "" {
		base = "machine"
	}
	used := make(map[string]bool, len(machines))
	for _, machine := range machines {
		used[strings.ToLower(machine.Alias)] = true
	}
	if !used[base] {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if !used[candidate] {
			return candidate
		}
	}
}
