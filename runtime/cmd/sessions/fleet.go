package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	fleetRequestTimeout      = 5 * time.Second
	localFleetRequestTimeout = 60 * time.Second
	localFleetEndpoint       = "local"
	// A search is answered by the local engine in milliseconds, and peers only
	// add to that answer. Peers therefore get a wall-clock budget rather than
	// the full request timeout: one unreachable LAN machine must not tax every
	// search the user or an agent runs.
	fleetPeerBudget = 750 * time.Millisecond
	// A peer that just failed will almost certainly fail again seconds later,
	// so skip it for a while instead of paying the budget on every query. The
	// window is short because a machine coming back is routine, not an event.
	fleetPeerCooldown = 5 * time.Minute
)

type fleetTarget struct {
	Alias    string
	Name     string
	Endpoint string
	Client   *apiClient
	Owned    bool
}

func fleetTargetTimeout(target fleetTarget) time.Duration {
	if target.Endpoint == localFleetEndpoint {
		// The first local search may need to build or upgrade a sizeable index.
		// Remote peers stay tightly bounded so one stale machine cannot stall the fleet.
		return localFleetRequestTimeout
	}
	return fleetRequestTimeout
}

func qualifiedHistoryReference(alias, historyID string) string {
	return alias + "::" + historyID
}

func splitQualifiedHistoryReference(value string) (string, string, bool) {
	alias, historyID, ok := strings.Cut(strings.TrimSpace(value), "::")
	alias = strings.TrimSpace(alias)
	historyID = strings.TrimSpace(historyID)
	return alias, historyID, ok && alias != "" && historyID != ""
}

func (a *app) useQualifiedHistoryReference(value *string) (string, error) {
	alias, historyID, qualified := splitQualifiedHistoryReference(*value)
	if !qualified {
		return "", nil
	}
	var client *apiClient
	var err error
	if strings.EqualFold(alias, "local") || strings.EqualFold(alias, "this-machine") {
		tokenPath, pathErr := sessionstate.LocalTokenPathFromEnv()
		if pathErr != nil {
			return "", pathErr
		}
		client, err = newAPIClient("127.0.0.1", a.port, tokenPath, true)
		alias = "local"
	} else {
		machine, machineErr := loadSavedMachine(a.home, alias)
		if machineErr != nil {
			return "", machineErr
		}
		client, err = newAPIClient(
			machine.Endpoint, "", savedMachineTokenPath(a.home, machine.MachineID), false,
		)
		alias = machine.Alias
	}
	if err != nil {
		return "", err
	}
	if a.api != nil {
		a.api.close()
	}
	a.api = client
	*value = historyID
	return alias, nil
}

func (a *app) approvedFleetTargets() ([]fleetTarget, error) {
	targets := []fleetTarget{{Alias: "local", Name: "This machine", Endpoint: localFleetEndpoint, Client: a.api}}
	registry, err := readMachineRegistry(a.home)
	if err != nil {
		return nil, err
	}
	for _, machine := range registry.Machines {
		client, clientErr := newAPIClient(
			machine.Endpoint, "", savedMachineTokenPath(a.home, machine.MachineID), false,
		)
		if clientErr != nil {
			continue
		}
		targets = append(targets, fleetTarget{
			Alias: machine.Alias, Name: machine.Name, Endpoint: machine.Endpoint,
			Client: client, Owned: true,
		})
	}
	return targets, nil
}

// fleetRequestError separates "this daemon rejected the request" from "this
// daemon could not be reached". Callers need that difference: a rejected
// request is the caller's to fix and will fail identically on every machine,
// while an unreachable machine says nothing at all about the request.
type fleetRequestError struct {
	path    string
	status  int
	message string
	cause   error
}

func (e *fleetRequestError) Error() string {
	switch {
	case e.cause != nil:
		return e.cause.Error()
	case e.message != "":
		return e.message
	default:
		return fmt.Sprintf("%s → %d", e.path, e.status)
	}
}

func (e *fleetRequestError) Unwrap() error { return e.cause }

// requestRejected reports a daemon that answered and refused: the request is
// wrong, not the machine.
func (e *fleetRequestError) requestRejected() bool {
	return e.status >= 400 && e.status < 500
}

func requestWasRejected(err error) (*fleetRequestError, bool) {
	var rejection *fleetRequestError
	if errors.As(err, &rejection) && rejection.requestRejected() {
		return rejection, true
	}
	return nil, false
}

func getJSONFromClient(client *apiClient, path string, target any, timeout time.Duration) error {
	response, err := client.request(context.Background(), http.MethodGet, path, nil, timeout)
	if err != nil {
		return &fleetRequestError{path: path, cause: err}
	}
	if response.status >= 400 {
		return &fleetRequestError{
			path: path, status: response.status, message: apiErrorMessage(response.body),
		}
	}
	return json.Unmarshal(response.body, target)
}

// apiErrorMessage prefers the daemon's own explanation. Instructional text is
// written where the operation failed; a status line reconstructed here would
// only describe the transport.
func apiErrorMessage(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error)
	}
	return strings.TrimSpace(prefixBytes(body, 200))
}

// fleetPeerHealth remembers which approved peers just failed. The CLI is a
// short-lived process, so the memory has to outlive it or every invocation
// rediscovers the same unreachable machine at full cost.
type fleetPeerHealth struct {
	Version int                         `json:"version"`
	Peers   map[string]fleetPeerFailure `json:"peers"`
	changed bool
}

type fleetPeerFailure struct {
	At    string `json:"at"`
	Error string `json:"error"`
}

const fleetPeerHealthVersion = 1

// fleetPeerHealthPath derives the cache location from the platform user state
// root rather than rebuilding the Unix layout, so Windows gets
// %LOCALAPPDATA%\Sessions\state instead of a literal ~/.local/state tree that
// means nothing there.
func fleetPeerHealthPath(home string) string {
	return filepath.Join(sessionstate.UserStateRootFor(home), "fleet-search-health.json")
}

// readFleetPeerHealth is deliberately forgiving: this file is an optimization,
// and an unreadable or stale-format cache must never keep a search from running.
func readFleetPeerHealth(home string) *fleetPeerHealth {
	health := &fleetPeerHealth{Version: fleetPeerHealthVersion, Peers: map[string]fleetPeerFailure{}}
	encoded, err := os.ReadFile(fleetPeerHealthPath(home))
	if err != nil {
		return health
	}
	var stored fleetPeerHealth
	if json.Unmarshal(encoded, &stored) != nil || stored.Version != fleetPeerHealthVersion {
		return health
	}
	if stored.Peers != nil {
		health.Peers = stored.Peers
	}
	return health
}

// coolingDown reports whether a peer failed recently enough to skip, and when
// it will be tried again.
func (h *fleetPeerHealth) coolingDown(alias string, now time.Time) (fleetPeerFailure, time.Time, bool) {
	failure, known := h.Peers[alias]
	if !known {
		return fleetPeerFailure{}, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, failure.At)
	if err != nil {
		return fleetPeerFailure{}, time.Time{}, false
	}
	retryAt := at.Add(fleetPeerCooldown)
	if !now.Before(retryAt) {
		return fleetPeerFailure{}, time.Time{}, false
	}
	return failure, retryAt, true
}

func (h *fleetPeerHealth) recordFailure(alias string, now time.Time, err error) {
	h.Peers[alias] = fleetPeerFailure{At: now.Format(time.RFC3339Nano), Error: err.Error()}
	h.changed = true
}

func (h *fleetPeerHealth) recordSuccess(alias string) {
	if _, known := h.Peers[alias]; !known {
		return
	}
	delete(h.Peers, alias)
	h.changed = true
}

// save is best effort for the same reason read is: losing the cache costs one
// slow search, while failing the command costs the answer.
func (h *fleetPeerHealth) save(home string) {
	if !h.changed {
		return
	}
	encoded, err := json.MarshalIndent(fleetPeerHealth{Version: fleetPeerHealthVersion, Peers: h.Peers}, "", "  ")
	if err != nil {
		return
	}
	_ = writePrivateFile(fleetPeerHealthPath(home), append(encoded, '\n'))
}
