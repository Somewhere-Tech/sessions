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
	// How long a peer gets to complete a TCP connection. A shut laptop and a
	// busy one are different failures and must cost differently: nothing has
	// been established with a machine that is powered off, so there is nothing
	// to wait for, while a machine already working on an answer deserves the
	// time that answer takes.
	//
	// It has to be shorter than the fan-out budget or the distinction never
	// surfaces — the budget fires first, and a powered-off machine is filed as
	// one that was busy listing a large history, which is both false and the
	// wrong shelf: it would be skipped as too slow rather than cooled down as
	// unreachable, and told the user it was still listing. A second is generous
	// for a handshake on a LAN or a tailnet, and the budget still bounds
	// anything slower.
	fleetPeerDialTimeout = time.Second
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
	// Listings remembers what each peer said the last time it answered a
	// history browse: how many conversations it held, and how long it took to
	// say so. Both are facts the next browse needs and neither can be
	// rediscovered without paying the peer's full cost again.
	//
	// The count is what makes a withheld peer reportable at its real scale. A
	// browse that drops a peer knows only that something is missing; "mini did
	// not answer" and "mini did not answer, and it held 1519 conversations an
	// hour ago" are the difference between a warning a reader can dismiss and
	// one they cannot.
	//
	// The duration is what makes the budget a measurement instead of a guess. A
	// peer's cost scales with the history it holds, so no number chosen once for
	// a fleet nobody had timed stays right; a peer that has proven it needs
	// three seconds is granted three seconds, and a peer that answers in eighty
	// milliseconds never widens anything.
	Listings map[string]fleetPeerListing `json:"listings,omitempty"`
	changed  bool
}

type fleetPeerFailure struct {
	At    string `json:"at"`
	Error string `json:"error"`
}

// fleetPeerListing is what one peer has shown about itself. At, Conversations
// and Counted describe its last complete answer. TookMS is separate and coarser:
// it is what the peer has shown it needs, which a complete answer measures
// exactly and an exceeded budget still bounds from below.
//
// Keeping the lower bound matters more than it looks. A peer whose answer costs
// more than any budget it is ever granted would otherwise never be measured at
// all — every browse would miss it, learn nothing, and miss it again — so the
// widening this whole mechanism exists for would never start on precisely the
// fleet that needs it. Recording "at least this long" makes each miss teach the
// next browse something, and the budget climbs to the peer's real cost within a
// few browses instead of never.
type fleetPeerListing struct {
	// At and Conversations are the last complete answer: a count, and when it
	// was taken. Only a browse that actually reached the peer writes them, so
	// the number reported for a missing machine is always a number that machine
	// really said.
	At            string `json:"at,omitempty"`
	Conversations int    `json:"conversations,omitempty"`
	Counted       bool   `json:"counted,omitempty"`
	// TookMS and SeenAt are what the peer costs: measured exactly by a complete
	// answer, bounded from below by a budget it exceeded, and stamped either
	// way. They are separate from the count because a browse that gave up
	// learned the cost and nothing else.
	TookMS int64  `json:"took_ms,omitempty"`
	SeenAt string `json:"seen_at,omitempty"`
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
	if stored.Listings != nil {
		health.Listings = stored.Listings
	}
	return health
}

// recordListing remembers a peer's answer so the next browse can price it and,
// if it is missed, still say how much was missed.
func (h *fleetPeerHealth) recordListing(alias string, now time.Time, conversations int, took time.Duration) {
	if h.Listings == nil {
		h.Listings = map[string]fleetPeerListing{}
	}
	stamp := now.Format(time.RFC3339Nano)
	h.Listings[alias] = fleetPeerListing{
		At: stamp, Conversations: conversations, Counted: true,
		TookMS: took.Milliseconds(), SeenAt: stamp,
	}
	h.changed = true
}

// recordSlow remembers that a peer was still answering when its budget ran out.
// It is a lower bound on that peer's cost, not a measurement, so it never
// touches the count: a browse that gave up learned nothing about how much the
// machine holds, and overwriting the last real count with a guess would be the
// one mistake this whole line exists to prevent.
func (h *fleetPeerHealth) recordSlow(alias string, now time.Time, budget time.Duration) {
	if h.Listings == nil {
		h.Listings = map[string]fleetPeerListing{}
	}
	listing := h.Listings[alias]
	if budget.Milliseconds() > listing.TookMS {
		listing.TookMS = budget.Milliseconds()
	}
	listing.SeenAt = now.Format(time.RFC3339Nano)
	h.Listings[alias] = listing
	h.changed = true
}

// lastListing reports what a peer held the last time it answered. The second
// value separates "this peer holds nothing" from "this peer has never been
// reached from here", which a browse must never print as the same sentence.
func (h *fleetPeerHealth) lastListing(alias string) (fleetPeerListing, time.Time, bool) {
	listing, known := h.Listings[alias]
	if !known {
		return fleetPeerListing{}, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, listing.At)
	if err != nil {
		return listing, time.Time{}, true
	}
	return listing, at, true
}

// costSeenAt is when this peer's cost was last observed, which paces the
// recheck. It is not when its conversations were counted: a peer skipped every
// ten minutes keeps refreshing what it costs while the count it reports stays
// as old as the last browse that truly reached it.
func (l fleetPeerListing) costSeenAt() time.Time {
	at, err := time.Parse(time.RFC3339Nano, l.SeenAt)
	if err != nil {
		return time.Time{}
	}
	return at
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
	encoded, err := json.MarshalIndent(fleetPeerHealth{
		Version: fleetPeerHealthVersion, Peers: h.Peers, Listings: h.Listings,
	}, "", "  ")
	if err != nil {
		return
	}
	_ = writePrivateFile(fleetPeerHealthPath(home), append(encoded, '\n'))
}
