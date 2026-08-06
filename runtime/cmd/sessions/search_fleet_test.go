package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

func searchMatchFixture(sessionID, name string) historysearch.Match {
	timestamp := "2026-08-01T12:00:00Z"
	return historysearch.Match{
		SessionID: sessionID, Name: name, Tool: "claude", Role: "user",
		MessageID: "message-" + sessionID, Timestamp: &timestamp, Snippet: "[[needle]] here",
	}
}

func searchResponseServer(t *testing.T, matches ...historysearch.Match) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(historysearch.Response{Matches: matches, Total: len(matches)})
	}))
	t.Cleanup(server.Close)
	return server
}

func fleetSearchApp(t *testing.T, localURL string, args ...string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	application, err := newApp(
		append([]string{"--json", "--host", localURL}, args...),
		strings.NewReader(""), stdout, stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.close)
	// The fleet fan-out is the default path; --host only supplies the local
	// endpoint the test daemon listens on.
	application.explicitTarget = false
	return application, stdout, stderr
}

// A search must cost what the local engine costs. An approved peer that has
// stopped answering is allowed to withhold its own matches, never to hold up
// the answer this machine already has.
func TestFleetSearchAnswersWithoutWaitingOutAStalledPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := searchResponseServer(t, searchMatchFixture("provider:claude:local", "Local notes"))
	release := make(chan struct{})
	var closed bool
	stalled := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		<-release
		_ = json.NewEncoder(response).Encode(historysearch.Response{Matches: []historysearch.Match{}})
	}))
	defer stalled.Close()
	defer func() {
		if !closed {
			close(release)
		}
	}()
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mac mini", Endpoint: stalled.URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	application, stdout, _ := fleetSearchApp(t, local.URL, "search", "needle")
	started := time.Now()
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	close(release)
	closed = true
	if elapsed > 3*time.Second {
		t.Fatalf("fleet search took %s; a stalled peer must not be waited out", elapsed)
	}

	var result historysearch.Response
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode fleet search: %v\n%s", err, stdout.String())
	}
	if len(result.Matches) != 1 || !result.Partial || len(result.Machines) != 2 {
		t.Fatalf("fleet result = %#v", result)
	}
	peer := result.Machines[1]
	if peer.Alias != "mini" || peer.Status != "unavailable" ||
		!strings.Contains(peer.Error, "did not answer within") ||
		!strings.Contains(peer.Error, "--machine mini") {
		t.Fatalf("dropped peer state = %#v", peer)
	}
}

// The second search in a row must not pay for the same dead peer again.
func TestFleetSearchSkipsAPeerThatJustFailed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := searchResponseServer(t, searchMatchFixture("provider:claude:local", "Local notes"))
	var requests atomic.Int64
	refusing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer refusing.Close()
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mac mini", Endpoint: refusing.URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		application, stdout, _ := fleetSearchApp(t, local.URL, "search", "needle")
		if err := application.dispatch(); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		var result historysearch.Response
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("attempt %d decode: %v\n%s", attempt, err, stdout.String())
		}
		if len(result.Machines) != 2 || !result.Partial {
			t.Fatalf("attempt %d machines = %#v", attempt, result.Machines)
		}
		skipped := strings.Contains(result.Machines[1].Error, "skipped after a recent failure")
		if attempt == 1 && skipped {
			t.Fatalf("first attempt must actually try the peer: %#v", result.Machines[1])
		}
		if attempt == 2 {
			if !skipped || !strings.Contains(result.Machines[1].Error, "--machine mini") {
				t.Fatalf("second attempt peer state = %#v", result.Machines[1])
			}
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("peer received %d requests, want 1: a peer that just failed must be skipped", got)
	}
	if _, err := os.Stat(fleetPeerHealthPath(home)); err != nil {
		t.Fatalf("peer health was not remembered across invocations: %v", err)
	}
}

// A machine that answers "your query is wrong" is a healthy machine. Reporting
// it as an unanswered fleet sends the reader to debug a working network.
func TestFleetSearchReportsARejectedQueryRatherThanBlamingTheNetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const guidance = `ranked search could not parse this query near "NOT". AND, OR, and NOT are ranked-search operators`
	rejecting := func() *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(response).Encode(map[string]string{"error": guidance})
		}))
		t.Cleanup(server.Close)
		return server
	}
	local := rejecting()
	for _, machine := range []savedMachine{
		{Alias: "mini", MachineID: "machine-mini", Name: "Mac mini", Endpoint: rejecting().URL},
		{Alias: "studio", MachineID: "machine-studio", Name: "Studio", Endpoint: rejecting().URL},
	} {
		if _, err := saveMachine(home, machine, "device-secret"); err != nil {
			t.Fatal(err)
		}
	}
	application, _, _ := fleetSearchApp(t, local.URL, "search", "NOT beta")
	err := application.dispatch()
	if err == nil {
		t.Fatal("a rejected query must fail the command")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 for a caller-fixable query", code)
	}
	if !strings.Contains(err.Error(), guidance) {
		t.Fatalf("error = %q, want the daemon's own instruction", err)
	}
	if strings.Contains(err.Error(), "no approved Sessions machine answered") {
		t.Fatalf("error = %q, want no LAN diagnosis for a query error", err)
	}
	// The peers are healthy, so nothing may be cached against them.
	if health := readFleetPeerHealth(home); len(health.Peers) != 0 {
		t.Fatalf("rejecting peers were cached as failures: %#v", health.Peers)
	}
}

// When the fleet really is unreachable, every machine that was tried has to be
// named, not just the single-machine case.
func TestFleetSearchNamesEveryUnreachableMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	failing := func() *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(response).Encode(map[string]string{"error": "sessionsd is restarting"})
		}))
		t.Cleanup(server.Close)
		return server
	}
	local := failing()
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mac mini", Endpoint: failing().URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}
	application, _, _ := fleetSearchApp(t, local.URL, "search", "needle")
	err := application.dispatch()
	if err == nil || exitCode(err) != 2 {
		t.Fatalf("error = %v, exit = %d, want a transport failure", err, exitCode(err))
	}
	for _, want := range []string{"local", "mini", "Mac mini", "sessionsd is restarting"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// An explicitly targeted search owes the same distinction as the fleet path.
func TestSingleDaemonSearchSeparatesRejectedQueriesFromUnreachableDaemons(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const guidance = "ranked search could not parse this query: a quote is opened and never closed."
	rejecting := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(response).Encode(map[string]string{"error": guidance})
	}))
	defer rejecting.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", rejecting.URL, "search", `"unterminated`}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), guidance) {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "400") {
		t.Fatalf("stderr=%q, want the instruction rather than the status line", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", "http://127.0.0.1:1", "search", "needle"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "search failed") {
		t.Fatalf("unreachable exit=%d stderr=%q", code, stderr.String())
	}
}

// The id column is the handle a reader hands back to `sessions cat`. Truncating
// a namespaced provider id to eight characters prints the word "provider" for
// every row and resolves to nothing.
func TestSearchIdentityColumnStaysUsableForProviderHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerID = "provider:codex:019f7b1c-0000-4000-8000-000000000001"
	server := searchResponseServer(t, searchMatchFixture(providerID, "Codex work"))
	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "search", "needle", "--exact"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), providerID+"  Codex work") {
		t.Fatalf("stdout = %q, want the full provider reference", stdout.String())
	}
	if searchMatchIdentity("aaaaaaaa-1111-4222-8333-444444444444") != "aaaaaaaa" {
		t.Fatal("opaque session ids keep their compact resolvable prefix")
	}
}
