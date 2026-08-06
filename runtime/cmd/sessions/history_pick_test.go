package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

// pickFixtureDaemon is a browse the picker can actually act against: it answers
// the listing and preview reads the browser already makes, and it accepts the
// one write a selection can produce, recording it so a test can assert what
// selection did rather than only what it printed.
type pickFixtureDaemon struct {
	server *httptest.Server
	mu     sync.Mutex
	writes []pickWrite
}

type pickWrite struct {
	path string
	body map[string]any
}

func newPickFixtureDaemon(
	t *testing.T, live []session, conversations []conversationFixture,
) *pickFixtureDaemon {
	t.Helper()
	daemon := &pickFixtureDaemon{}
	byID := make(map[string]conversationFixture, len(conversations))
	listing := integrations.HistoryResponse{SchemaVersion: integrations.SchemaVersion}
	for _, conversation := range conversations {
		byID[conversation.session.ID] = conversation
		listing.Sessions = append(listing.Sessions, conversation.session)
	}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			payload, _ := io.ReadAll(request.Body)
			decoded := map[string]any{}
			_ = json.Unmarshal(payload, &decoded)
			daemon.mu.Lock()
			daemon.writes = append(daemon.writes, pickWrite{path: request.URL.Path, body: decoded})
			daemon.mu.Unlock()
			_ = json.NewEncoder(response).Encode(recovery.AdoptResult{OK: true, LaneID: "lane-reopened"})
			return
		}
		switch {
		case request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(sessionsResponse{Sessions: live})
		case request.URL.Path == "/api/history":
			_ = json.NewEncoder(response).Encode(listing)
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

func (d *pickFixtureDaemon) recordedWrites() []pickWrite {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]pickWrite(nil), d.writes...)
}

// runPickCLI drives the interactive loop itself with scripted keystrokes, which
// is the only way this feature is actually proven: the loop, its prompt, its
// refusals and the command it finally runs are all exercised end to end.
func runPickCLI(
	t *testing.T, daemon *pickFixtureDaemon, keystrokes string, args ...string,
) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--host", daemon.server.URL}, args...),
		strings.NewReader(keystrokes), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func pickConversations(now time.Time) []conversationFixture {
	return []conversationFixture{
		{
			session: conversationAt("provider:codex:newest", "ship the release", "codex", "/w/one", 12, now.Add(-time.Hour)),
			messages: []integrations.TranscriptMessage{
				{Role: "user", Text: "cut the release branch"},
				{Role: "assistant", Text: "tagged v0.2.16 and pushed"},
			},
		},
		{
			session: conversationAt("provider:claude:middle", "rewrite the parser", "claude", "/w/two", 7, now.Add(-5*time.Hour)),
			messages: []integrations.TranscriptMessage{
				{Role: "user", Text: "the parser drops trailing flags"},
				{Role: "assistant", Text: "fixed by consuming the option before the positional"},
			},
		},
		{
			session:  conversationAt("provider:codex:oldest", "audit the sockets", "codex", "/w/three", 4, now.Add(-30*time.Hour)),
			messages: []integrations.TranscriptMessage{{Role: "user", Text: "check the socket path length"}},
		},
	}
}

// The whole point: the row's command no longer has to be copied. Choosing a
// row runs it, and what ran is asserted against the daemon, not against prose.
func TestPickReopensTheChosenConversation(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	stdout, stderr, code := runPickCLI(t, daemon, "2\ny\n", "history", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "1. ship the release") || !strings.Contains(stdout, "2. rewrite the parser") {
		t.Fatalf("rows were not numbered for selection: %q", stdout)
	}
	if !strings.Contains(stderr, "Reopen which conversation? 1-3") {
		t.Fatalf("no selection prompt was shown: %q", stderr)
	}
	// The confirmation names the conversation and the exact command, so a
	// mistyped number is caught while it is still free to catch.
	if !strings.Contains(stderr, "Resume rewrite the parser (provider:claude:middle)?") ||
		!strings.Contains(stderr, "sessions resume provider:claude:middle") {
		t.Fatalf("selection was not confirmed with its command: %q", stderr)
	}
	writes := daemon.recordedWrites()
	if len(writes) != 1 || writes[0].path != "/api/recovery/adopt" {
		t.Fatalf("selection did not resume exactly once: %#v", writes)
	}
	if writes[0].body["historyId"] != "provider:claude:middle" {
		t.Fatalf("selection resumed the wrong conversation: %#v", writes[0].body)
	}
	if !strings.Contains(stdout, "lane-reopened") {
		t.Fatalf("the reopened session was not reported: %q", stdout)
	}
}

// Looking at a candidate and then going back to the list is the thing neither
// provider's own picker gives you, so it is tested as a round trip: preview,
// return, list again, then choose.
func TestPickPreviewsThenReturnsToTheList(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	stdout, stderr, code := runPickCLI(t, daemon, "p 2\nl\n2\ny\n", "history", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "2. rewrite the parser") {
		t.Fatalf("preview did not identify the row it read: %q", stdout)
	}
	if !strings.Contains(stdout, "the parser drops trailing flags") ||
		!strings.Contains(stdout, "fixed by consuming the option before the positional") {
		t.Fatalf("preview did not show the conversation's tail: %q", stdout)
	}
	// The list came back after the preview scrolled it away: the first row's
	// numbered line is printed once for the browse and once for `l`.
	if strings.Count(stdout, "1. ship the release") != 2 {
		t.Fatalf("the list was not reprinted after the preview: %q", stdout)
	}
	// Previewing did not resume anything, and the later selection did.
	writes := daemon.recordedWrites()
	if len(writes) != 1 || writes[0].body["historyId"] != "provider:claude:middle" {
		t.Fatalf("preview-then-pick wrote the wrong things: %#v", writes)
	}
	if strings.Count(stderr, "Reopen which conversation?") != 3 {
		t.Fatalf("the loop did not return to the prompt after each step: %q", stderr)
	}
}

// A picker that acted on the number alone would resume the wrong conversation
// on a fat-fingered keystroke, so declining leaves the loop running and nothing
// created.
func TestPickDoesNothingWhenTheConfirmationIsDeclined(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	_, stderr, code := runPickCLI(t, daemon, "1\nn\nq\n", "history", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if writes := daemon.recordedWrites(); len(writes) != 0 {
		t.Fatalf("a declined confirmation still resumed something: %#v", writes)
	}
	// Declining returns to the list rather than exiting, so a second prompt was
	// offered after the refusal.
	if strings.Count(stderr, "Reopen which conversation?") != 2 {
		t.Fatalf("declining did not return to the picker: %q", stderr)
	}
}

func TestPickQuitsWithoutSelecting(t *testing.T) {
	now := time.Now()
	for name, keystrokes := range map[string]string{
		"q":         "q\n",
		"blankLine": "\n",
		"eof":       "",
	} {
		t.Run(name, func(t *testing.T) {
			daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
			_, stderr, code := runPickCLI(t, daemon, keystrokes, "history", "--pick")
			if code != 0 {
				t.Fatalf("quitting a picker is not a failure: exit=%d stderr=%q", code, stderr)
			}
			if writes := daemon.recordedWrites(); len(writes) != 0 {
				t.Fatalf("quitting resumed something: %#v", writes)
			}
		})
	}
}

// A live conversation must attach, never resume: the daemon refuses resume on
// one, so a picker that resumed it would fail on exactly the conversation a
// user is most likely to pick. Attach needs a real terminal, which a scripted
// test is not — that refusal is the proof it was attach that ran.
func TestPickAttachesALiveConversationInsteadOfResumingIt(t *testing.T) {
	now := time.Now()
	const id = "11111111-2222-4333-8444-555555555555"
	daemon := newPickFixtureDaemon(t,
		[]session{{ID: id, Cmd: "claude", Tool: "claude-code"}},
		[]conversationFixture{{session: conversationAt(id, "running now", "claude", "/w/live", 9, now)}})
	_, stderr, code := runPickCLI(t, daemon, "1\ny\n", "history", "--pick")
	if !strings.Contains(stderr, "Attach to running now") {
		t.Fatalf("a live row was not offered as an attach: %q", stderr)
	}
	if !strings.Contains(stderr, "sessions attach 11111111") {
		t.Fatalf("selection did not run the attach command the row printed: %q", stderr)
	}
	if !strings.Contains(stderr, "attach requires an interactive terminal") || code != 2 {
		t.Fatalf("attach did not run: exit=%d stderr=%q", code, stderr)
	}
	if writes := daemon.recordedWrites(); len(writes) != 0 {
		t.Fatalf("a live conversation was resumed: %#v", writes)
	}
}

// A row that carries no command carries none because nothing can bring it back.
// Selecting it is refused with that row's own reason, rather than dispatched
// into a failure, and previewing it says the same thing.
func TestPickRefusesAConversationNothingCanReopen(t *testing.T) {
	now := time.Now()
	lost := conversationAt("provider:claude:gone", "pruned away", "claude", "/w/gone", 3, now)
	lost.ConversationAvailable = false
	daemon := newPickFixtureDaemon(t, nil, []conversationFixture{{session: lost}})
	stdout, stderr, code := runPickCLI(t, daemon, "1\np 1\nq\n", "history", "--all", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "1. pruned away") {
		t.Fatalf("an unreopenable row lost its number, so the numbering no longer matches the screen: %q", stdout)
	}
	if !strings.Contains(stderr,
		"pruned away (provider:claude:gone) cannot be reopened: neither the provider nor Sessions still holds this conversation") {
		t.Fatalf("selection was not refused with the row's own reason: %q", stderr)
	}
	if !strings.Contains(stderr, "no preview for 1. pruned away") {
		t.Fatalf("previewing an unreopenable row did not say why: %q", stderr)
	}
	if writes := daemon.recordedWrites(); len(writes) != 0 {
		t.Fatalf("an unreopenable row was dispatched anyway: %#v", writes)
	}
}

// Mistyping at the prompt re-asks with the grammar rather than acting on a
// guess or dropping out of the loop.
func TestPickRefusesInputItCannotRead(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	_, stderr, code := runPickCLI(t, daemon, "banana\n9\np 0\n3\ny\n", "history", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		`"banana" is not one of 1-3`,
		`"9" is not one of 1-3`,
		`"0" is not one of 1-3`,
		"type a row number, p N to preview it, l to list again, or q to quit",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("bad input was not explained (%q): %q", want, stderr)
		}
	}
	writes := daemon.recordedWrites()
	if len(writes) != 1 || writes[0].body["historyId"] != "provider:codex:oldest" {
		t.Fatalf("the loop did not survive bad input to reach a real choice: %#v", writes)
	}
}

// The protection every scripted caller depends on: nothing about this command
// becomes interactive on its own. Without --pick the same keystrokes on stdin
// are ignored, the output carries no numbers and no prompt, and stdin is never
// read at all.
func TestHistoryIsNotInteractiveWithoutPick(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	stdout, stderr, code := runPickCLI(t, daemon, "1\ny\n", "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "Reopen which conversation?") {
		t.Fatalf("history prompted without being asked to: %q", stderr)
	}
	if strings.Contains(stdout, "1. ship the release") {
		t.Fatalf("history numbered its rows without being asked to: %q", stdout)
	}
	if !strings.Contains(stdout, "sessions resume provider:codex:newest") {
		t.Fatalf("the per-row command is still the answer for a non-interactive caller: %q", stdout)
	}
	if writes := daemon.recordedWrites(); len(writes) != 0 {
		t.Fatalf("a non-interactive browse resumed something: %#v", writes)
	}
	// The offer to pick is advice for a person at a terminal, and this caller
	// is a pipe, so it is not printed either.
	if strings.Contains(stdout, "--pick") {
		t.Fatalf("a piped caller was advertised an interactive mode: %q", stdout)
	}
}

// A JSON caller must never be handed a prompt it will not answer on a stream
// that never ends. The combination is refused outright rather than silently
// dropping the flag, which would leave the caller believing it had a picker.
func TestPickIsRefusedWithJSON(t *testing.T) {
	now := time.Now()
	daemon := newPickFixtureDaemon(t, nil, pickConversations(now))
	stdout, stderr, code := runPickCLI(t, daemon, "1\ny\n", "--json", "history", "--pick")
	if code != 1 {
		t.Fatalf("--pick --json was accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "--pick is interactive; it cannot be combined with --json") {
		t.Fatalf("the refusal was not reported in the JSON the caller asked for: %q", stdout)
	}
	if writes := daemon.recordedWrites(); len(writes) != 0 {
		t.Fatalf("a refused command still acted: %#v", writes)
	}
	// And the same refusal when --json is written after the command.
	_, _, code = runPickCLI(t, daemon, "1\ny\n", "history", "--pick", "--json")
	if code != 1 {
		t.Fatalf("--pick --json was accepted in the trailing spelling: exit=%d", code)
	}
}

// An empty browse has nothing to pick from, and must not sit at a prompt that
// can only be answered with q.
func TestPickDoesNotPromptOverAnEmptyBrowse(t *testing.T) {
	daemon := newPickFixtureDaemon(t, nil, nil)
	stdout, stderr, code := runPickCLI(t, daemon, "", "history", "--pick")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "Reopen which conversation?") {
		t.Fatalf("an empty browse prompted for a selection: %q", stderr)
	}
	if !strings.Contains(stdout, "no conversations matched") {
		t.Fatalf("an empty browse lost its advice under --pick: %q", stdout)
	}
}
