package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

// conversationFixture is one row of a fake daemon's history.
type conversationFixture struct {
	session  integrations.HistorySession
	messages []integrations.TranscriptMessage
}

type historyFixtureDaemon struct {
	server *httptest.Server
	mu     sync.Mutex
	// requests records every method+path the CLI issued, so a test can assert
	// that browsing and previewing never wrote anything.
	requests []string
	search   historysearch.Response
}

func newHistoryFixtureDaemon(
	t *testing.T, live []session, conversations []conversationFixture,
) *historyFixtureDaemon {
	t.Helper()
	daemon := &historyFixtureDaemon{}
	byID := make(map[string]conversationFixture, len(conversations))
	listing := integrations.HistoryResponse{SchemaVersion: integrations.SchemaVersion}
	for _, conversation := range conversations {
		byID[conversation.session.ID] = conversation
		listing.Sessions = append(listing.Sessions, conversation.session)
	}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		daemon.mu.Lock()
		daemon.requests = append(daemon.requests, request.Method+" "+request.URL.Path)
		daemon.mu.Unlock()
		switch {
		case request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(sessionsResponse{Sessions: live})
		case request.URL.Path == "/api/history":
			_ = json.NewEncoder(response).Encode(listing)
		case request.URL.Path == "/api/search":
			daemon.mu.Lock()
			answer := daemon.search
			daemon.mu.Unlock()
			_ = json.NewEncoder(response).Encode(answer)
		case strings.HasSuffix(request.URL.Path, "/preview"):
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/history/"), "/preview")
			conversation, known := byID[id]
			if !known {
				http.NotFound(response, request)
				return
			}
			_ = json.NewEncoder(response).Encode(integrations.TranscriptResponse{
				SchemaVersion: integrations.SchemaVersion,
				Session:       conversation.session, Messages: conversation.messages,
			})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.server.Close)
	return daemon
}

func (d *historyFixtureDaemon) methods() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func conversationAt(id, name, tool, cwd string, messages int, at time.Time) integrations.HistorySession {
	return integrations.HistorySession{
		ID: id, Name: name, Tool: tool, CWD: cwd, MessageCount: messages,
		LastActivityAt: at.UnixMilli(), ConversationUpdatedAt: at.UnixMilli(),
		ConversationAvailable: true,
	}
}

func runHistoryCLI(t *testing.T, daemon *historyFixtureDaemon, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--host", daemon.server.URL}, args...),
		strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// runFleetHistoryCLI browses the approved fleet rather than one pinned daemon:
// --host only supplies the local endpoint, exactly as fleetSearchApp does for
// search, and the peer comes from the saved machine registry under home.
func runFleetHistoryCLI(
	t *testing.T, home string, local *historyFixtureDaemon, args ...string,
) (string, string, error) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	application, err := newApp(
		append([]string{"--host", local.server.URL}, args...), strings.NewReader(""), stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.close)
	application.explicitTarget = false
	dispatchErr := application.dispatch()
	return stdout.String(), stderr.String(), dispatchErr
}

// stalledPeer is an approved machine that has accepted the request and not
// answered it — the shape of a peer holding a large history, which is the case
// a browse gets wrong. It never answers until the test says so, so nothing here
// depends on how long anything takes.
type stalledPeer struct {
	server  *httptest.Server
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	paths   []string
}

func newStalledPeer(t *testing.T) *stalledPeer {
	t.Helper()
	peer := &stalledPeer{started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	peer.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		peer.mu.Lock()
		peer.paths = append(peer.paths, request.URL.Path)
		peer.mu.Unlock()
		if request.URL.Path == "/api/history" {
			once.Do(func() { close(peer.started) })
			<-peer.release
		}
		_ = json.NewEncoder(response).Encode(integrations.HistoryResponse{
			SchemaVersion: integrations.SchemaVersion,
		})
	}))
	t.Cleanup(func() {
		select {
		case <-peer.release:
		default:
			close(peer.release)
		}
		peer.server.Close()
	})
	return peer
}

// requestPaths is every request this peer has received, so a test can prove a
// browse did not ask it anything rather than inferring it from the clock.
func (p *stalledPeer) requestPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func (p *stalledPeer) awaitRequest(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the peer never received the history request")
	}
}

// approvePeer registers one peer in home's machine registry.
func approvePeer(t *testing.T, home, alias, endpoint string) {
	t.Helper()
	if _, err := saveMachine(home, savedMachine{
		Alias: alias, MachineID: "machine-" + alias, Name: "Mac mini", Endpoint: endpoint,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}
}

// The defect this fixes, in the shape it was measured: a two-machine fleet
// where the peer holds five sixths of the history and does not answer in time.
// The browse then reported 306 conversations recorded and 291 matching, with a
// warning on stderr naming the machine — which a reader has no way to price and
// a redirected browse never sees at all. The counts on stdout have to say that
// they are not the fleet, and say how much of it is missing.
func TestHistoryFooterSaysHowMuchAWithheldMachineHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, now)},
	})
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)

	// mini answered a browse an hour ago and held 1519 conversations then. That
	// is the only number available without paying the round trip that was just
	// missed, and it is the number the reader needs.
	health := readFleetPeerHealth(home)
	health.recordListing("mini", now.Add(-time.Hour), 1519, 3500*time.Millisecond)
	health.save(home)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history")
	if err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	peer.awaitRequest(t)

	if !strings.Contains(stdout, "Not the whole fleet: mini is missing") {
		t.Fatalf("stdout never said the answer was short a machine:\n%s", stdout)
	}
	if !strings.Contains(stdout, "held 1519 conversations when it last answered") {
		t.Fatalf("stdout never said how much was withheld:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 conversations recorded on the machines that answered") {
		t.Fatalf("the footer still states a partial count as the corpus:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--wait-for-peers") {
		t.Fatalf("stdout offered no way to get the whole answer:\n%s", stdout)
	}
	// The existing diagnostic keeps its place: stdout says what it cost, stderr
	// says why. Losing the second would make an unreachable peer undebuggable.
	if !strings.Contains(stderr, "mini") {
		t.Fatalf("stderr no longer names the unavailable machine: %q", stderr)
	}
}

// The count in that line has to be an observation, not a fixture. A browse that
// reaches a peer records what it held, and the next browse that misses it
// reports that number back.
func TestHistoryRemembersWhatAPeerHeldSoAMissedBrowseCanPriceIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, now)},
	})
	answering := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:far1", "peer one", "codex", "/w/far", 5, now)},
		{session: conversationAt("provider:codex:far2", "peer two", "codex", "/w/far", 6, now)},
	})
	approvePeer(t, home, "mini", answering.server.URL)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history")
	if err != nil {
		t.Fatalf("first browse failed: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "peer one") || strings.Contains(stdout, "Not the whole fleet") {
		t.Fatalf("a peer that answered was not merged into the browse:\n%s", stdout)
	}
	listing, _, known := readFleetPeerHealth(home).lastListing("mini")
	if !known || listing.Conversations != 2 {
		t.Fatalf("the peer's answer was not remembered: %#v (known %v)", listing, known)
	}
	if listing.TookMS < 0 {
		t.Fatalf("the peer's cost was not measured: %#v", listing)
	}
}

// A machine nobody here has ever reached is a different sentence. Printing a
// count for it would invent one, and printing nothing would leave the reader
// with a browse that looks complete.
func TestHistoryAdmitsWhenAWithheldMachinesScaleIsUnknown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, time.Now())},
	})
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history")
	if err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	peer.awaitRequest(t)
	if !strings.Contains(stdout, "how many conversations it holds has never been recorded here") {
		t.Fatalf("an uncounted machine was reported as though it were counted:\n%s", stdout)
	}
	if strings.Contains(stdout, "held 0 conversations") {
		t.Fatalf("never reached was printed as holding nothing:\n%s", stdout)
	}
}

// An empty answer is the most misleading place of all to omit this: "no
// conversations matched" reads as a fact about the fleet.
func TestHistoryEmptyAnswerStillNamesTheMachineThatDidNotAnswer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := newHistoryFixtureDaemon(t, nil, nil)
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)
	health := readFleetPeerHealth(home)
	health.recordListing("mini", time.Now().Add(-time.Minute), 1519, 3500*time.Millisecond)
	health.save(home)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history")
	if err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "Not the whole fleet: mini is missing") ||
		!strings.Contains(stdout, "1519 conversations") {
		t.Fatalf("an empty browse presented itself as an empty fleet:\n%s", stdout)
	}
}

// --json callers read `known` as the corpus. They need the same correction the
// footer got, in a field rather than a sentence.
func TestHistoryJSONCarriesWhatWasWithheld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, time.Now())},
	})
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)
	health := readFleetPeerHealth(home)
	health.recordListing("mini", time.Now().Add(-time.Minute), 1519, 3500*time.Millisecond)
	health.save(home)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "--json", "history")
	if err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	var answer historyBrowseResponse
	if decodeErr := json.Unmarshal([]byte(stdout), &answer); decodeErr != nil {
		t.Fatalf("history --json was not decodable: %v (%q)", decodeErr, stdout)
	}
	if !answer.Partial || len(answer.Withheld) != 1 {
		t.Fatalf("withheld machines missing from the document: %#v", answer)
	}
	missing := answer.Withheld[0]
	if missing.Alias != "mini" || !missing.Counted || missing.Conversations != 1519 ||
		missing.CountedAt == "" || missing.Reason == "" {
		t.Fatalf("withheld entry = %#v", missing)
	}
}

// The second half of the fix: stop paying for the same failure. A peer that has
// shown it cannot answer inside the budget is left out at no cost instead of
// being waited for at full cost on every browse, and it is re-tried on a timer
// so a machine that got faster comes back on its own.
func TestHistoryDoesNotPayTheBudgetForAPeerItKnowsIsTooSlow(t *testing.T) {
	now := time.Now()
	health := &fleetPeerHealth{Peers: map[string]fleetPeerFailure{}}

	if _, tooSlow := peerCannotAnswerInTime(health, "mini", now); tooSlow {
		t.Fatal("a peer nobody has timed was skipped; the first browse has to look")
	}

	health.recordListing("mini", now, 12, 80*time.Millisecond)
	if _, tooSlow := peerCannotAnswerInTime(health, "mini", now); tooSlow {
		t.Fatal("a peer that answers in 80ms was skipped")
	}

	health.recordListing("mini", now, 1519, 3700*time.Millisecond)
	listing, tooSlow := peerCannotAnswerInTime(health, "mini", now)
	if !tooSlow {
		t.Fatal("a peer measured at 3.7s is still asked, and the browse still pays 2s to be told so")
	}
	if listing.Conversations != 1519 {
		t.Fatalf("the skip lost the count that prices it: %#v", listing)
	}

	// Not a verdict: the machine is asked again once the window passes.
	if _, tooSlow := peerCannotAnswerInTime(
		health, "mini", now.Add(fleetHistoryPeerRecheck+time.Minute)); tooSlow {
		t.Fatal("a peer written off permanently can never come back")
	}
}

// A miss has to teach the next browse something, or nothing ever learns that
// this peer is too slow and every browse keeps paying the budget to find out.
// It also must not be cooled down: being large is not being unreachable.
func TestHistoryLearnsFromAPeerThatMissedItsBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, time.Now())},
	})
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)

	if _, stderr, err := runFleetHistoryCLI(t, home, local, "history"); err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	peer.awaitRequest(t)

	health := readFleetPeerHealth(home)
	if _, _, cooling := health.coolingDown("mini", time.Now()); cooling {
		t.Fatal("a peer that was still answering was cooled down as though it were unreachable")
	}
	listing, _, known := health.lastListing("mini")
	if !known || listing.TookMS < fleetHistoryPeerBudget.Milliseconds() {
		t.Fatalf("the miss taught the next browse nothing: %#v (known %v)", listing, known)
	}
	if listing.Counted {
		t.Fatalf("a browse that gave up invented a conversation count: %#v", listing)
	}
	if _, tooSlow := peerCannotAnswerInTime(health, "mini", time.Now()); !tooSlow {
		t.Fatal("the next browse would pay the whole budget again to learn the same thing")
	}

	// And it does not: the second browse neither contacts the peer nor waits for
	// it, while still reporting that the answer is short a machine.
	before := len(peer.requestPaths())
	started := time.Now()
	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history")
	if err != nil {
		t.Fatalf("second browse failed: %v (stderr %q)", err, stderr)
	}
	if got := len(peer.requestPaths()); got != before {
		t.Fatalf("the second browse asked the peer again: %d requests, want %d", got, before)
	}
	if elapsed := time.Since(started); elapsed >= fleetHistoryPeerBudget {
		t.Fatalf("the second browse still paid %s for a peer it did not ask", elapsed)
	}
	if !strings.Contains(stdout, "Not the whole fleet: mini is missing") {
		t.Fatalf("a skipped peer vanished from the answer instead of being reported:\n%s", stdout)
	}
	if !strings.Contains(stderr, "not asked") {
		t.Fatalf("stderr did not distinguish a peer left out from one that failed: %q", stderr)
	}
}

// --wait-for-peers is the explicit lever the shortfall line advertises. It has
// to actually wait: a caller that asked for the whole fleet and got the fast
// partial answer anyway is worse off than before, because now they believe they
// asked.
func TestHistoryWaitForPeersWaitsForTheMachineTheBudgetWouldDrop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, now)},
	})
	peer := newStalledPeer(t)
	approvePeer(t, home, "mini", peer.server.URL)
	// mini failed a browse a moment ago, which is exactly when a reader reaches
	// for this flag. The cooldown that keeps the fast path fast must not answer
	// --wait-for-peers with the shortfall line that recommended it.
	health := readFleetPeerHealth(home)
	health.recordFailure("mini", now, errPending)
	health.save(home)

	type answer struct {
		stdout string
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		stdout, _, err := runFleetHistoryCLI(t, home, local, "history", "--wait-for-peers")
		answers <- answer{stdout: stdout, err: err}
	}()
	peer.awaitRequest(t)

	// The peer is now inside the request and will stay there until this test
	// lets it out, so "the browse has not finished" is a fact rather than a
	// race: under the wall-clock budget it would have given up and printed a
	// partial answer well inside this window, and under --wait-for-peers it
	// cannot finish at all. Nothing here sleeps waiting for a hoped-for state.
	select {
	case got := <-answers:
		t.Fatalf("--wait-for-peers gave up on a peer that was still answering:\n%s", got.stdout)
	case <-time.After(2 * fleetHistoryPeerBudget):
	}
	close(peer.release)

	select {
	case got := <-answers:
		if got.err != nil {
			t.Fatalf("--wait-for-peers failed: %v", got.err)
		}
		if strings.Contains(got.stdout, "Not the whole fleet") {
			t.Fatalf("--wait-for-peers still dropped the peer:\n%s", got.stdout)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("--wait-for-peers never returned after the peer answered")
	}
}

// A machine that is out of disk, or otherwise answering with its own failure,
// is not reached by waiting longer. The shortfall line must not send a caller
// who already used the flag back around to use it again.
func TestHistoryDoesNotRecommendWaitingToACallerWhoAlreadyWaited(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:here", "local work", "codex", "/w/here", 3, time.Now())},
	})
	failing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(response).Encode(map[string]string{"error": "no space left on device"})
	}))
	defer failing.Close()
	approvePeer(t, home, "mini", failing.URL)

	stdout, stderr, err := runFleetHistoryCLI(t, home, local, "history", "--wait-for-peers")
	if err != nil {
		t.Fatalf("browse failed: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "Not the whole fleet: mini is missing") {
		t.Fatalf("a peer answering with its own failure was not reported:\n%s", stdout)
	}
	if strings.Contains(stdout, "Add --wait-for-peers") {
		t.Fatalf("the answer told a caller who waited to wait:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sessions --machine mini history") {
		t.Fatalf("no next step was offered for a peer waiting cannot reach:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("the peer's own explanation was lost: %q", stderr)
	}
}

// The complaint this command exists for: a conversation started somewhere else
// and closed, which the user cannot find again. Browsing must need no search
// term, must order by recency, and must hand back a command per row.
func TestHistoryBrowsesWithoutAQuery(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:new", "ship the release", "codex", "/w/one", 12, now.Add(-2*time.Hour))},
		{session: conversationAt("provider:codex:old", "older codex work", "codex", "/w/two", 4, now.Add(-40*time.Hour))},
		{session: conversationAt("provider:claude:mid", "claude thing", "claude", "/w/three", 7, now.Add(-6*time.Hour))},
	})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--tool", "codex")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "ship the release") || !strings.Contains(stdout, "older codex work") {
		t.Fatalf("browse dropped a codex conversation: %q", stdout)
	}
	if strings.Contains(stdout, "claude thing") {
		t.Fatalf("--tool codex leaked a claude conversation: %q", stdout)
	}
	if strings.Index(stdout, "ship the release") > strings.Index(stdout, "older codex work") {
		t.Fatalf("rows are not newest-first: %q", stdout)
	}
	if !strings.Contains(stdout, "sessions resume provider:codex:new") {
		t.Fatalf("row carried no resume command: %q", stdout)
	}
	// Enough to recognise it without opening it.
	for _, want := range []string{"12 messages", "codex", "/w/one"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("row is missing %q: %q", want, stdout)
		}
	}
}

// A conversation that is running right now is not resumable — Sessions' own
// guard refuses it and says to attach — so the browser must print attach for
// it. Printing resume there would be a command that fails on exactly the
// conversation a user is most likely to pick.
func TestHistoryPrintsAttachForALiveConversation(t *testing.T) {
	now := time.Now()
	const id = "11111111-2222-4333-8444-555555555555"
	daemon := newHistoryFixtureDaemon(t,
		[]session{{ID: id, Cmd: "claude", Tool: "claude-code"}},
		[]conversationFixture{
			{session: conversationAt(id, "running now", "claude", "/w/live", 9, now)},
		})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "sessions resume") {
		t.Fatalf("live conversation offered resume, which the daemon refuses: %q", stdout)
	}
	if !strings.Contains(stdout, "sessions attach 11111111") || !strings.Contains(stdout, "LIVE NOW") {
		t.Fatalf("live conversation was not marked or not attachable: %q", stdout)
	}
}

// A conversation nothing still holds gets no command at all and says why,
// matching `sessions recover`'s rule that a printed command is one that works.
func TestHistoryRefusesToInventACommandForALostConversation(t *testing.T) {
	now := time.Now()
	lost := conversationAt("provider:claude:gone", "pruned away", "claude", "/w/gone", 3, now)
	lost.ConversationAvailable = false
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{session: lost}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--all")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "sessions resume") {
		t.Fatalf("unrecoverable conversation was offered a resume command: %q", stdout)
	}
	if !strings.Contains(stdout, "cannot be resumed") {
		t.Fatalf("unrecoverable conversation gave no reason: %q", stdout)
	}
	// And it is not in the default view at all, which lists what you can return to.
	stdout, _, _ = runHistoryCLI(t, daemon, "history")
	if strings.Contains(stdout, "pruned away") {
		t.Fatalf("default browse listed a conversation nothing can reopen: %q", stdout)
	}
	if !strings.Contains(stdout, "1 conversations are recorded") {
		t.Fatalf("empty browse hid the fact that history exists: %q", stdout)
	}
}

// Recency has to follow the conversation, not the record. One housekeeping
// sweep can stamp every finished session's record with the same instant; the
// transcripts did not change, and it is the transcripts the user remembers.
func TestHistoryOrdersByConversationActivityNotRecordActivity(t *testing.T) {
	now := time.Now()
	real := conversationAt("provider:codex:real", "the one you want", "codex", "/w/real", 2065, now.Add(-6*time.Hour))
	swept := conversationAt("sess-swept", "finished test lane", "codex", "/w/testing", 2, now.Add(-9*time.Hour))
	// The sweep moved the record forward; the conversation itself did not move.
	swept.LastActivityAt = now.Add(-1 * time.Minute).UnixMilli()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{session: swept}, {session: real}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Index(stdout, "the one you want") > strings.Index(stdout, "finished test lane") {
		t.Fatalf("record housekeeping outranked real conversation activity: %q", stdout)
	}
}

// Preview reads; it must not create or mark anything.
func TestHistoryPreviewShowsTheTailAndWritesNothing(t *testing.T) {
	now := time.Now()
	timestamp := "2026-08-05T12:00:00Z"
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{
		session: conversationAt("provider:codex:one", "candidate", "codex", "/w/one", 4, now),
		messages: []integrations.TranscriptMessage{
			{Role: "user", Text: "first thing"},
			{Role: "tool", Kind: "bash", Text: "noise that is not a conversation"},
			{Role: "assistant", Text: "middle answer"},
			{Role: "user", Text: "last question", Timestamp: &timestamp},
			{Role: "assistant", Text: "last answer"},
		},
	}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--preview", "2")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "last question") || !strings.Contains(stdout, "last answer") {
		t.Fatalf("preview did not show the end of the conversation: %q", stdout)
	}
	if strings.Contains(stdout, "first thing") {
		t.Fatalf("--preview 2 showed more than two exchanges: %q", stdout)
	}
	if strings.Contains(stdout, "noise that is not a conversation") {
		t.Fatalf("preview showed tool traffic rather than the conversation: %q", stdout)
	}
	for _, request := range daemon.methods() {
		if !strings.HasPrefix(request, "GET ") {
			t.Fatalf("browsing or previewing issued %q; it must only read", request)
		}
	}
}

// A conversation is not a message. When a query narrows the browse, the answer
// is still one row per conversation, and it must survive a daemon whose search
// route does not send the per-session rollup.
func TestHistoryQueryNarrowsToConversationsWithoutARollup(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:hit", "the hardening lane", "codex", "/w/hit", 30, now)},
		{session: conversationAt("provider:codex:miss", "unrelated", "codex", "/w/miss", 5, now.Add(-time.Hour))},
	})
	// Deliberately no Sessions rollup, the way an older daemon answers.
	daemon.search = historysearch.Response{
		Matches: []historysearch.Match{
			{SessionID: "provider:codex:hit", Name: "the hardening lane", Snippet: "[[hardening]] the runner"},
			{SessionID: "provider:codex:hit", Name: "the hardening lane", Snippet: "more [[hardening]]"},
		},
		Total: 2,
	}
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "hardening")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "the hardening lane") {
		t.Fatalf("query dropped the conversation that matched: %q", stdout)
	}
	if strings.Contains(stdout, "unrelated") {
		t.Fatalf("query kept a conversation that did not match: %q", stdout)
	}
	if strings.Count(stdout, "sessions resume provider:codex:hit") != 1 {
		t.Fatalf("two matching messages produced more than one conversation row: %q", stdout)
	}
	if !strings.Contains(stdout, "1 conversation matches") {
		t.Fatalf("footer did not report the conversation count: %q", stdout)
	}
}

// The rollup is the better source when it is there: it counts every hit rather
// than the ones that reached the page.
func TestHistoryQueryPrefersTheSessionRollupWhenPresent(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:hit", "the hardening lane", "codex", "/w/hit", 30, now)},
	})
	daemon.search = historysearch.Response{
		Sessions: []historysearch.SessionHits{{
			SessionID: "provider:codex:hit", Name: "the hardening lane", Hits: 42,
			Snippets: []string{"rollup snippet"},
		}},
	}
	stdout, _, code := runHistoryCLI(t, daemon, "history", "hardening")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "42 matching messages") || !strings.Contains(stdout, "rollup snippet") {
		t.Fatalf("rollup evidence was not used: %q", stdout)
	}
}

func TestHistoryJSONIsOneDocumentWithTheDocumentedShape(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:one", "candidate", "codex", "/w/one", 4, now)},
		{session: conversationAt("provider:codex:two", "other", "codex", "/w/two", 6, now.Add(-time.Hour))},
	})
	stdout, stderr, code := runHistoryCLI(t, daemon, "--json", "history", "-n", "1")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var answer historyBrowseResponse
	if err := decoder.Decode(&answer); err != nil {
		t.Fatalf("history --json was not decodable: %v (%q)", err, stdout)
	}
	if decoder.More() {
		t.Fatal("history --json wrote more than one document")
	}
	if answer.SchemaVersion != integrations.SchemaVersion {
		t.Fatalf("schemaVersion = %d", answer.SchemaVersion)
	}
	if answer.Known != 2 || answer.Matched != 2 || answer.Shown != 1 {
		t.Fatalf("counts = known %d matched %d shown %d, want 2/2/1", answer.Known, answer.Matched, answer.Shown)
	}
	if len(answer.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(answer.Conversations))
	}
	row := answer.Conversations[0]
	if row.Status != historyStatusResumable ||
		strings.Join(row.Resume, " ") != "sessions resume provider:codex:one" {
		t.Fatalf("row = %#v", row)
	}
	if row.LastActiveAt == "" || row.LastActiveAtMS == 0 || row.Messages != 4 {
		t.Fatalf("row lacks what identifies it: %#v", row)
	}
}

func TestHistoryJSONFailureCarriesTheExitCode(t *testing.T) {
	daemon := newHistoryFixtureDaemon(t, nil, nil)
	stdout, _, code := runHistoryCLI(t, daemon, "--json", "history", "--tool", "emacs")
	if code == 0 {
		t.Fatal("an invalid --tool exited 0")
	}
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(stdout), &failure); err != nil {
		t.Fatalf("failure was not JSON: %v (%q)", err, stdout)
	}
	if failure.OK || failure.Code != code || failure.Error == "" {
		t.Fatalf("failure document = %#v, exit %d", failure, code)
	}
}

// `sessions search --since today --tool codex` used to be refused as a typo:
// "the query must come before flags". The user asking it cannot supply a word,
// because not remembering one is why they are asking. The refusal now hands
// back the command that answers the question.
func TestSearchWithOnlyFiltersHandsBackTheBrowseCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"search", "--since", "today", "--tool", "codex"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "sessions history --since today --tool codex") {
		t.Fatalf("refusal did not hand back a working command: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "must come before flags") {
		t.Fatalf("refusal still reads as a syntax complaint: %q", stderr.String())
	}
}

// A real mistyped query keeps its old, correct explanation.
func TestSearchStillExplainsAFlagUsedAsTheQuery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"search", "--nonsense", "words"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "must come before flags") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestParseHistoryTimeUnderstandsHowPeopleSayWhen(t *testing.T) {
	now := time.Date(2026, 8, 5, 21, 30, 0, 0, time.Local)
	startOfToday := time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local)
	tests := []struct {
		raw      string
		endOfDay bool
		want     time.Time
	}{
		{raw: "today", want: startOfToday},
		{raw: "yesterday", want: startOfToday.AddDate(0, 0, -1)},
		{raw: "today", endOfDay: true, want: startOfToday.AddDate(0, 0, 1)},
		{raw: "3d", want: now.Add(-72 * time.Hour)},
		{raw: "6h", want: now.Add(-6 * time.Hour)},
		{raw: "2w", want: now.Add(-14 * 24 * time.Hour)},
		{raw: "2026-07-01", want: time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)},
		{raw: "2026-07-01T10:00:00Z", want: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)},
	}
	for _, test := range tests {
		got, label, err := parseHistoryTime(test.raw, now, test.endOfDay)
		if err != nil {
			t.Fatalf("%q: %v", test.raw, err)
		}
		if got != test.want.UnixMilli() {
			t.Fatalf("%q = %d, want %d", test.raw, got, test.want.UnixMilli())
		}
		if label == "" {
			t.Fatalf("%q produced no description of the bound", test.raw)
		}
	}
	if _, _, err := parseHistoryTime("last tuesday", now, false); err == nil {
		t.Fatal("an unreadable date was accepted")
	}
}

// --preview alone is one word; --preview 8 is the same option with the count
// given. A following flag must never be swallowed as the count.
func TestPluckOptionalCountKeepsFollowingFlags(t *testing.T) {
	args := []string{"--preview", "--tool", "codex"}
	count, present, err := pluckOptionalCount(&args, "--preview", 4, 20)
	if err != nil || !present || count != 4 {
		t.Fatalf("count=%d present=%v err=%v", count, present, err)
	}
	if strings.Join(args, " ") != "--tool codex" {
		t.Fatalf("remaining args = %q", args)
	}
	args = []string{"--preview", "8", "--tool", "codex"}
	count, present, err = pluckOptionalCount(&args, "--preview", 4, 20)
	if err != nil || !present || count != 8 {
		t.Fatalf("count=%d present=%v err=%v", count, present, err)
	}
	if strings.Join(args, " ") != "--tool codex" {
		t.Fatalf("remaining args = %q", args)
	}
	args = []string{"--preview", "999"}
	if _, _, err = pluckOptionalCount(&args, "--preview", 4, 20); err == nil {
		t.Fatal("an out-of-range count was accepted")
	}
}
