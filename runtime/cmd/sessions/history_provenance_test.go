package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func withSurface(
	session integrations.HistorySession, surface watch.ConversationSurface,
) integrations.HistorySession {
	session.Surface = &surface
	return session
}

// The sentence the user has to be able to read off a row: "Codex Desktop on
// this machine, 2h ago, ~/pretty-PTY, you started it", as against "Codex CLI,
// yesterday, automation". Neither provider's picker can say either of those.
func TestHistoryRowShowsWhereAConversationCameFrom(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: withSurface(
			conversationAt("provider:codex:desk", "hardening sweep", "codex", "/w/pretty-PTY", 412, now.Add(-2*time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexDesktop, Label: "Codex Desktop",
				Originator: "Codex Desktop", Source: "vscode",
				Actor: watch.ActorUser, ActorRaw: "user",
			})},
		{session: withSurface(
			conversationAt("provider:codex:auto", "nightly sweep", "codex", "/w/bolo", 30, now.Add(-26*time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexCLI, Label: "Codex CLI",
				Originator: "codex-tui", Source: "cli",
				Actor: watch.ActorAutomation, ActorRaw: "automation",
			})},
	})

	stdout, _, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s", code, stdout)
	}
	if !strings.Contains(stdout, "Codex Desktop") || !strings.Contains(stdout, "Codex CLI") {
		t.Fatalf("rows do not name the surface:\n%s", stdout)
	}
	// The surface takes the provider column rather than sitting beside it, so
	// the row does not spend a field restating what the label already says.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "Codex Desktop") && strings.Contains(line, " · codex · ") {
			t.Fatalf("row repeats the provider next to the surface: %q", line)
		}
	}
	// The exception is flagged; the ordinary case stays silent.
	automation := lineContaining(t, stdout, "Codex CLI")
	if !strings.Contains(automation, "automation") {
		t.Fatalf("an automation row did not say so: %q", automation)
	}
	desktop := lineContaining(t, stdout, "Codex Desktop")
	if strings.Contains(desktop, "user") || strings.Contains(desktop, "automation") {
		t.Fatalf("a row the user drove spent ink saying so: %q", desktop)
	}
}

func TestHistoryFiltersBySurfaceAndActor(t *testing.T) {
	now := time.Now()
	fixtures := []conversationFixture{
		{session: withSurface(
			conversationAt("provider:codex:desk", "desktop work", "codex", "/w/one", 12, now.Add(-time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexDesktop, Label: "Codex Desktop",
				Originator: "Codex Desktop", Actor: watch.ActorUser,
			})},
		{session: withSurface(
			conversationAt("provider:codex:tui", "terminal work", "codex", "/w/two", 9, now.Add(-2*time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexCLI, Label: "Codex CLI",
				Originator: "codex-tui", Actor: watch.ActorAutomation,
			})},
		{session: withSurface(
			conversationAt("provider:codex:own", "sessions work", "codex", "/w/three", 5, now.Add(-3*time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceSessions, Label: "Codex via Sessions",
				Originator: "pretty-pty",
			})},
	}
	daemon := newHistoryFixtureDaemon(t, nil, fixtures)

	for _, test := range []struct {
		args    []string
		want    string
		exclude []string
	}{
		{args: []string{"--surface", "codex-desktop"}, want: "desktop work", exclude: []string{"terminal work", "sessions work"}},
		{args: []string{"--surface", "Codex Desktop"}, want: "desktop work", exclude: []string{"terminal work"}},
		{args: []string{"--surface", "pretty-pty"}, want: "sessions work", exclude: []string{"desktop work"}},
		{args: []string{"--surface", "sessions"}, want: "sessions work", exclude: []string{"terminal work"}},
		{args: []string{"--actor", "automation"}, want: "terminal work", exclude: []string{"desktop work", "sessions work"}},
		{args: []string{"--actor", "me"}, want: "desktop work", exclude: []string{"terminal work", "sessions work"}},
	} {
		stdout, _, code := runHistoryCLI(t, daemon, append([]string{"history"}, test.args...)...)
		if code != 0 {
			t.Fatalf("%v exit=%d stdout=%s", test.args, code, stdout)
		}
		if !strings.Contains(stdout, test.want) {
			t.Fatalf("%v did not keep %q:\n%s", test.args, test.want, stdout)
		}
		for _, unwanted := range test.exclude {
			if strings.Contains(stdout, unwanted) {
				t.Fatalf("%v kept %q:\n%s", test.args, unwanted, stdout)
			}
		}
	}

	// An unrecognised surface is accepted rather than refused -- a provider can
	// add an originator tomorrow -- so an empty answer has to say what is
	// actually here instead of leaving the reader to guess at a typo.
	stdout, _, code := runHistoryCLI(t, daemon, "history", "--surface", "codex-destkop")
	if code != 0 {
		t.Fatalf("misspelled surface: exit=%d stdout=%s", code, stdout)
	}
	if !strings.Contains(stdout, "Surfaces recorded here:") ||
		!strings.Contains(stdout, "codex-desktop") || !strings.Contains(stdout, "pretty-pty") {
		t.Fatalf("a misspelled surface did not report what exists:\n%s", stdout)
	}

	_, stderr, code := runHistoryCLI(t, daemon, "history", "--surface", "   ")
	if code != 1 || !strings.Contains(stderr, "--surface needs a surface") {
		t.Fatalf("blank surface: exit=%d stderr=%s", code, stderr)
	}
	_, stderr, code = runHistoryCLI(t, daemon, "history", "--actor", "nonsense")
	if code != 1 || !strings.Contains(stderr, "--actor must be") {
		t.Fatalf("bad actor: exit=%d stderr=%s", code, stderr)
	}
}

// The daemon on the other end may be older than the field. Provenance is
// additive, so it answers with none of it, and a filtered browse must not read
// that silence as "you have no Desktop conversations".
func TestHistorySaysWhenADaemonCannotReportProvenance(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:one", "older daemon row", "codex", "/w/one", 12, now.Add(-time.Hour))},
		{session: conversationAt("provider:codex:two", "another older row", "codex", "/w/two", 8, now.Add(-2*time.Hour))},
	})

	// Unfiltered, the rows are unaffected: they simply carry no surface and the
	// provider name is what the row shows, exactly as before.
	stdout, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "older daemon row") || !strings.Contains(stdout, "codex") {
		t.Fatalf("an old daemon's rows stopped listing:\n%s", stdout)
	}
	if strings.Contains(stderr, "does not report") {
		t.Fatalf("an unfiltered browse warned about provenance: %s", stderr)
	}

	stdout, stderr, code = runHistoryCLI(t, daemon, "history", "--surface", "codex-desktop")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "older daemon row") {
		t.Fatalf("a row with no provenance matched a surface filter:\n%s", stdout)
	}
	if !strings.Contains(stderr, "2 conversations were left out") ||
		!strings.Contains(stderr, "does not report where a conversation was started") {
		t.Fatalf("the shortfall was not explained: %s", stderr)
	}

	var browse historyBrowseResponse
	stdout, _, code = runHistoryCLI(t, daemon, "--json", "history", "--actor", "user")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s", code, stdout)
	}
	if err := json.Unmarshal([]byte(stdout), &browse); err != nil {
		t.Fatal(err)
	}
	if browse.ProvenanceUnreported != 2 {
		t.Fatalf("provenance_unreported = %d, want 2: %s", browse.ProvenanceUnreported, stdout)
	}
}

func TestHistoryJSONCarriesRawAndNormalizedProvenance(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: withSurface(
			conversationAt("provider:codex:desk", "desktop work", "codex", "/w/one", 12, now.Add(-time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexDesktop, Label: "Codex Desktop",
				Originator: "codex_work_desktop", Source: "vscode",
				Actor: watch.ActorAgent, ActorRaw: "subagent", Version: "0.104.0",
			})},
	})
	stdout, _, code := runHistoryCLI(t, daemon, "--json", "history")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s", code, stdout)
	}
	var browse historyBrowseResponse
	if err := json.Unmarshal([]byte(stdout), &browse); err != nil {
		t.Fatal(err)
	}
	if len(browse.Conversations) != 1 {
		t.Fatalf("conversations = %#v", browse.Conversations)
	}
	row := browse.Conversations[0]
	if row.Surface != "Codex Desktop" || row.SurfaceKind != watch.SurfaceCodexDesktop ||
		row.SurfaceRaw != "codex_work_desktop" || row.Actor != watch.ActorAgent {
		t.Fatalf("row provenance = %#v", row)
	}
}

// A row dated by its file rather than by its own last record says so, because a
// history copied without preserving timestamps otherwise presents every
// conversation as having just happened.
func TestHistoryMarksAFileTimeRow(t *testing.T) {
	now := time.Now()
	fromFile := conversationAt("provider:codex:copy", "copied conversation", "codex", "/w/one", 3, now.Add(-time.Hour))
	fromRecord := conversationAt("provider:codex:real", "recorded conversation", "codex", "/w/two", 4, now.Add(-2*time.Hour))
	fromFile.ConversationUpdatedApproximate = true
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: fromFile}, {session: fromRecord},
	})
	stdout, _, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s", code, stdout)
	}
	if !strings.Contains(lineContaining(t, stdout, "ago"), "ago") {
		t.Fatalf("no age on any row:\n%s", stdout)
	}
	copied := metaLineAfter(t, stdout, "copied conversation")
	if !strings.Contains(copied, "(file time)") {
		t.Fatalf("a file-dated row did not say so: %q", copied)
	}
	// "recorded conversation" is also exactly what a daemon too old to have the
	// field looks like on the wire: a timestamp with no claim about where it
	// came from. Both must go unmarked, because branding every row of an old
	// daemon's answer "(file time)" would be a fabrication in the opposite
	// direction from the one the mark exists to prevent.
	recorded := metaLineAfter(t, stdout, "recorded conversation")
	if strings.Contains(recorded, "(file time)") {
		t.Fatalf("a row that made no claim was branded file-dated: %q", recorded)
	}
}

func lineContaining(t *testing.T, output, needle string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", needle, output)
	return ""
}

// metaLineAfter returns the meta line printed under a named conversation.
func metaLineAfter(t *testing.T, output, name string) string {
	t.Helper()
	lines := strings.Split(output, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == name && index+1 < len(lines) {
			return lines[index+1]
		}
	}
	t.Fatalf("no row named %q in:\n%s", name, output)
	return ""
}

// The two reasons a row can lack provenance are not interchangeable. A machine
// running an older Sessions can be updated; a conversation recovered from
// Claude's prompt archive never carried launch context and never will. On the
// development machine 81 of the 91 excluded rows were archive records, answered
// by a daemon reporting provenance perfectly well for the other 212 — telling
// the user to update it would have sent them to fix nothing.
func TestHistorySeparatesAnOldDaemonFromAnArchiveWithNoProvenance(t *testing.T) {
	now := time.Now()
	archived := conversationAt("provider-history:claude:one", "archived prompts", "claude", "/w/one", 6, now.Add(-time.Hour))
	archived.PromptHistoryOnly = true
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: archived},
		{session: withSurface(
			conversationAt("provider:codex:desk", "desktop work", "codex", "/w/two", 12, now.Add(-2*time.Hour)),
			watch.ConversationSurface{
				Kind: watch.SurfaceCodexDesktop, Label: "Codex Desktop",
				Originator: "Codex Desktop", Actor: watch.ActorUser,
			})},
	})

	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--surface", "codex-desktop")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "desktop work") {
		t.Fatalf("the matching row was lost:\n%s", stdout)
	}
	if !strings.Contains(stderr, "nothing recorded where they were started") {
		t.Fatalf("the archive row was not explained: %s", stderr)
	}
	// This daemon plainly can answer -- it answered for the other row -- so it
	// must not be blamed.
	if strings.Contains(stderr, "Update Sessions there") {
		t.Fatalf("a capable daemon was blamed for an archive record: %s", stderr)
	}
}
