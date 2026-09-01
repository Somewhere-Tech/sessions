package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func filterConversations(
	rows []conversationRow, filters historyFilters,
) (kept, withoutProvenance []conversationRow) {
	kept = make([]conversationRow, 0, len(rows))
	for _, row := range rows {
		if filters.tool != "" && row.Tool != filters.tool {
			continue
		}
		if filters.wantsProvenance() && !row.hasProvenance() {
			withoutProvenance = append(withoutProvenance, row)
			continue
		}
		if filters.surface != "" && !row.matchesSurface(filters.surface) {
			continue
		}
		if filters.actor != "" && row.Actor != filters.actor {
			continue
		}
		if !filters.all && !filters.explicit {
			if row.Tool != "claude" && row.Tool != "codex" {
				continue
			}
		}
		if !filters.all {
			// An uncounted row is not an empty one. A daemon answering from its
			// cache reports no count for a transcript it did not parse, and
			// dropping those would hide the conversations most likely to be the
			// ones being looked for.
			if (row.Messages <= 0 && !row.MessagesUncounted) ||
				row.Status == historyStatusUnrecoverable || row.Status == historyStatusUnreadable {
				continue
			}
		}
		if filters.cwd != "" && !withinDirectory(row.CWD, filters.cwd) {
			continue
		}
		if filters.nameGlob != "" {
			matched, err := filepath.Match(strings.ToLower(filters.nameGlob), strings.ToLower(row.Name))
			if err != nil || !matched {
				continue
			}
		}
		if len(filters.sessions) > 0 && !matchesAnyConversationID(row, filters.sessions) {
			continue
		}
		// A conversation nobody has spoken into cannot be one somebody has, and
		// a conversation with no live session cannot say either way, so both are
		// excluded rather than guessed at.
		if filters.touched && row.LastHumanMessageAtMS <= 0 {
			continue
		}
		// A conversation with no recorded activity time cannot be placed on a
		// timeline, so a date filter has to exclude it rather than guess.
		if filters.sinceMS != 0 && row.LastActiveAtMS < filters.sinceMS {
			continue
		}
		if filters.untilMS != 0 && (row.LastActiveAtMS == 0 || row.LastActiveAtMS >= filters.untilMS) {
			continue
		}
		kept = append(kept, row)
	}
	return kept, withoutProvenance
}

// hasProvenance reports whether the answering daemon said anything at all about
// where this conversation came from.
func (r conversationRow) hasProvenance() bool {
	return r.SurfaceKind != "" || r.SurfaceRaw != "" || r.Actor != ""
}

// matchesSurface accepts the token, the raw provider value, or the label, all
// folded the same way. A person who read "Codex Desktop" in a row should be
// able to type it back, and a person who saw the raw `pretty-pty` in --json
// should be able to select on that too.
func (r conversationRow) matchesSurface(wanted string) bool {
	for _, candidate := range []string{r.SurfaceKind, r.SurfaceRaw, r.Surface} {
		if candidate == "" {
			continue
		}
		if wanted == watch.NormalizeSurfaceKind(candidate) {
			return true
		}
	}
	return false
}

func matchesAnyConversationID(row conversationRow, wanted []string) bool {
	for _, value := range wanted {
		if row.ID == value || row.Reference == value {
			return true
		}
		if _, id, qualified := splitQualifiedHistoryReference(value); qualified && row.ID == id {
			return true
		}
		if len(value) >= 4 && strings.HasPrefix(row.ID, value) {
			return true
		}
	}
	return false
}

func withinDirectory(candidate, root string) bool {
	if candidate == "" {
		return false
	}
	want := filepath.Clean(root)
	got := filepath.Clean(candidate)
	return got == want || strings.HasPrefix(got, want+string(filepath.Separator))
}

// attachConversationPreviews reads the tail of each shown conversation through
// the same history reader `sessions cat` uses. It is a read: no session is
// created, nothing is marked, and nothing about the conversation changes.
func (a *app) attachConversationPreviews(rows []conversationRow, targets []fleetTarget, count int) {
	if len(rows) == 0 {
		return
	}
	work := make(chan int)
	var wait sync.WaitGroup
	workers := min(historyPreviewConcurrency, len(rows))
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				row := &rows[index]
				if len(row.Resume) == 0 {
					row.PreviewError = row.Reason
					continue
				}
				messages, err := readConversationTail(targets[row.target], row.ID, count)
				if err != nil {
					row.PreviewError = err.Error()
					continue
				}
				row.Preview = messages
			}
		}()
	}
	for index := range rows {
		work <- index
	}
	close(work)
	wait.Wait()
}

// readConversationTail asks the history preview view for the end of a
// conversation. The preview route is already tail-bounded, so the last few
// exchanges never require pulling a two-thousand-message transcript across.
func readConversationTail(target fleetTarget, id string, count int) ([]conversationPreviewMessage, error) {
	var transcript integrations.TranscriptResponse
	path := "/api/history/" + escapeID(id) + "/preview?format=json"
	if err := getJSONFromClient(target.Client, path, &transcript, fleetTargetTimeout(target)); err != nil {
		return nil, err
	}
	// Tool traffic is not what a person reads a conversation back for, and it
	// is most of the volume in an agent transcript.
	spoken := make([]integrations.TranscriptMessage, 0, len(transcript.Messages))
	for _, message := range transcript.Messages {
		if message.Role == "user" || message.Role == "assistant" {
			spoken = append(spoken, message)
		}
	}
	if len(spoken) > count {
		spoken = spoken[len(spoken)-count:]
	}
	preview := make([]conversationPreviewMessage, 0, len(spoken))
	for _, message := range spoken {
		entry := conversationPreviewMessage{Role: message.Role, Text: compactSearchText(message.Text)}
		if message.Timestamp != nil {
			entry.Timestamp = *message.Timestamp
		}
		preview = append(preview, entry)
	}
	return preview, nil
}

// numbered adds the row numbers the picker selects by. Every shown row is
// numbered, including the ones nothing can reopen: the numbers have to agree
// with what is on the screen, and a list whose numbering skipped rows would
// make "row 4" mean two different things depending on how carefully you
// counted. Selecting an unreopenable number is refused with that row's reason
// instead.
func (a *app) writeConversationRows(
	rows []conversationRow, matched, known int, query string, filters historyFilters,
	withoutProvenance []conversationRow, answered map[string]bool, numbered bool,
	withheld []withheldMachine, waited bool,
) error {
	if len(rows) == 0 {
		if _, err := io.WriteString(a.stdout,
			emptyConversationAdvice(known, query, filters, len(withheld) > 0)); err != nil {
			return err
		}
		if err := a.writeWithheldMachines(withheld, waited); err != nil {
			return err
		}
		return a.writeProvenanceShortfall(answered, withoutProvenance, filters)
	}
	for index, row := range rows {
		label := ""
		if numbered {
			label = fmt.Sprintf("%d. ", index+1)
		}
		if _, err := fmt.Fprintf(a.stdout, "%s%s\n", label, conversationName(row)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(a.stdout, "  %s\n", a.conversationMetaLine(row)); err != nil {
			return err
		}
		for _, snippet := range firstSnippets(row.Snippets) {
			if _, err := fmt.Fprintf(a.stdout, "  … %s\n", truncateRunes(compactSearchText(snippet), historySnippetRunes)); err != nil {
				return err
			}
		}
		for _, message := range row.Preview {
			if _, err := fmt.Fprintf(a.stdout, "  %-10s %s\n",
				message.Role, truncateRunes(message.Text, historySnippetRunes)); err != nil {
				return err
			}
		}
		if row.PreviewError != "" {
			if _, err := fmt.Fprintf(a.stdout, "  (preview unavailable: %s)\n", row.PreviewError); err != nil {
				return err
			}
		}
		line := "  " + shellRecipe(row.Resume)
		if len(row.Resume) == 0 {
			line = "  (cannot be resumed: " + row.Reason + ")"
		}
		if _, err := fmt.Fprintf(a.stdout, "%s\n\n", line); err != nil {
			return err
		}
	}
	if err := a.writeConversationFooter(
		len(rows), matched, known, query, filters, numbered, len(withheld) > 0); err != nil {
		return err
	}
	if err := a.writeWithheldMachines(withheld, waited); err != nil {
		return err
	}
	return a.writeProvenanceShortfall(answered, withoutProvenance, filters)
}

// writeWithheldMachines is the line that stops a partial browse from reading
// like a complete one.
//
// The counts above it are honest about what was searched and say nothing about
// what was not, and that is exactly how a browse of 306 conversations came to
// present itself as the whole of a fleet holding 1825. There was a warning —
// naming the machine, on stderr — and it was not enough, for two reasons that
// both had to be fixed. It went to a stream that `sessions history > list.txt`
// discards, so the saved answer carried no trace of the omission at all. And it
// said only that a machine was unavailable, which a reader has no way to price:
// a laptop that was asleep and held nothing reads identically to the machine
// holding five sixths of their history.
//
// So this goes on stdout beside the counts it corrects, and it carries the
// number. The number is the last count that machine reported here rather than a
// live one, because a live one costs exactly the round trip that was just
// missed; it is stated as of when it was taken, so it is never mistaken for a
// fact about now.
func (a *app) writeWithheldMachines(withheld []withheldMachine, waited bool) error {
	for _, machine := range withheld {
		scale := "and how many conversations it holds has never been recorded here"
		if machine.Counted {
			scale = fmt.Sprintf("and held %s when it last answered", conversationTotal(machine.Conversations))
			if machine.countedAtMS > 0 {
				scale += ", " + a.ageOf(machine.countedAtMS) + " ago"
			}
		}
		// A caller who already waited has spent the only lever this line has to
		// offer, so it points at the machine's own error instead of at itself.
		advice := "Add --wait-for-peers to include it."
		if waited {
			advice = fmt.Sprintf(
				"Waiting did not reach it; run `sessions --machine %s history` for its own answer.", machine.Alias)
		}
		if _, err := fmt.Fprintf(a.stdout,
			"Not the whole fleet: %s is missing, %s. %s\n",
			machine.Alias, scale, advice); err != nil {
			return err
		}
	}
	return nil
}

func conversationTotal(count int) string {
	if count == 1 {
		return "1 conversation"
	}
	return fmt.Sprintf("%d conversations", count)
}

// writeProvenanceShortfall says when a surface or actor filter came up short
// because nothing could answer the question, rather than because no
// conversation matched. A browse that silently omitted those rows would read as
// a confident "you have none of those" when the honest answer is "these could
// not say".
//
// The two reasons are not interchangeable and must not share a sentence. A
// machine running a Sessions older than the field returns every row with no
// provenance, and the fix is to update that machine. A conversation recovered
// from Claude's prompt archive has no provenance because the archive keeps
// prompts and nothing else, and no update will ever change that. Blaming the
// daemon for the second sends the reader to fix something that is not broken —
// which is exactly what this message did against the development machine's own
// history, where 81 of the 91 excluded rows were archive records answered by a
// daemon that reports provenance perfectly well for the other 212.
func (a *app) writeProvenanceShortfall(
	answered map[string]bool, withoutProvenance []conversationRow, filters historyFilters,
) error {
	if len(withoutProvenance) == 0 || !filters.wantsProvenance() {
		return nil
	}
	flag := "--surface"
	if filters.surface == "" {
		flag = "--actor"
	}
	staleMachines := make(map[string]struct{}, 2)
	stale, unrecorded := 0, 0
	for _, row := range withoutProvenance {
		if !answered[row.Machine] {
			stale++
			staleMachines[row.Machine] = struct{}{}
			continue
		}
		unrecorded++
	}
	if stale > 0 {
		machines := make([]string, 0, len(staleMachines))
		for machine := range staleMachines {
			machines = append(machines, machine)
		}
		sort.Strings(machines)
		if _, err := fmt.Fprintf(a.stderr,
			"sessions: %s left out of this %s answer: %s does not report where a conversation was started. Update Sessions there, or drop %s to see them.\n",
			conversationCount(stale), flag, strings.Join(machines, ", "), flag); err != nil {
			return err
		}
	}
	if unrecorded > 0 {
		_, err := fmt.Fprintf(a.stderr,
			"sessions: %s left out of this %s answer: nothing recorded where they were started. Claude's prompt archive keeps prompts only, and a shell record has no provider launch context at all.\n",
			conversationCount(unrecorded), flag)
		return err
	}
	return nil
}

// machinesReportingProvenance reports which machines answered with any
// provenance at all. A machine that produced even one surface is new enough to
// have looked, so its blank rows are the provider's silence rather than the
// daemon's version.
func machinesReportingProvenance(rows []conversationRow) map[string]bool {
	answered := make(map[string]bool, 4)
	for _, row := range rows {
		if row.hasProvenance() {
			answered[row.Machine] = true
		}
	}
	return answered
}

func conversationCount(count int) string {
	if count == 1 {
		return "1 conversation was"
	}
	return fmt.Sprintf("%d conversations were", count)
}

// conversationMetaLine is the one compact line under a conversation's name, and
// everything on it has to earn its place.
//
// The surface takes the provider column rather than sitting beside it. Every
// surface label names its provider — "Codex Desktop", "Claude Code", "Codex via
// Sessions" — so nothing is lost, and printing "codex · Codex Desktop" would
// spend a second field restating the first. When no surface was recorded the
// provider name is what remains, which is exactly today's row.
//
// The actor is printed only when it was something other than the user. A row
// that says "automation" or "agent" is telling the reader something they could
// not otherwise know; a row that says "user" is telling them what they already
// assumed about their own history, on every line. So the exception is flagged
// and the ordinary case stays silent — and --actor user remains for when the
// distinction has to be exact, since it matches only conversations that
// recorded it.
//
// Deliberately not here: the client version (Codex cli_version, Claude
// version), the raw originator, the git branch, and the recorded source value.
// They are real provenance and they are all carried in --json and in `sessions
// source`, but none of them is how a person recognises a conversation a week
// later, and a meta line long enough to wrap is one nobody reads.
// startedOnAnEarlierDay reports the start date when the conversation began on a
// different calendar day from its last activity, which is the case where the
// two timestamps carry different information. Both are read in local time,
// because the question being asked -- "was this the one from last Tuesday?" --
// is asked about the local day.
func (row conversationRow) startedOnAnEarlierDay() (string, bool) {
	if row.StartedAtMS <= 0 || row.LastActiveAtMS <= 0 {
		return "", false
	}
	started := time.UnixMilli(row.StartedAtMS)
	last := time.UnixMilli(row.LastActiveAtMS)
	startedDay := started.Format("2006-01-02")
	if startedDay == last.Format("2006-01-02") || started.After(last) {
		return "", false
	}
	return startedDay, true
}

func (a *app) conversationMetaLine(row conversationRow) string {
	parts := make([]string, 0, 8)
	when := "no recorded activity"
	if row.LastActiveAtMS > 0 {
		when = fmt.Sprintf("%s · %s ago",
			time.UnixMilli(row.LastActiveAtMS).Format("2006-01-02 15:04"), a.ageOf(row.LastActiveAtMS))
		if row.ApproximateTime {
			// The conversation never stamped its own last record, so this is the
			// file's modification time. Say so rather than let a copied history
			// present itself as a freshly used one.
			when += " (file time)"
		}
	}
	origin := row.Tool
	if row.Surface != "" {
		origin = row.Surface
	}
	parts = append(parts, when)
	// Only worth a column when it says something the last-active stamp does
	// not. A session started and finished this afternoon repeating its own
	// date is noise on every row; one started three weeks ago and touched
	// yesterday is the whole reason the user could not place it.
	if started, ok := row.startedOnAnEarlierDay(); ok {
		parts = append(parts, "started "+started)
	}
	parts = append(parts, origin, row.messageCountText())
	if row.CWD != "" {
		parts = append(parts, a.shortenHome(row.CWD))
	}
	if row.Machine != "" && row.Machine != "local" {
		parts = append(parts, "on "+row.Machine)
	}
	if row.Actor != "" && row.Actor != "user" {
		parts = append(parts, row.Actor)
	}
	if row.Status == historyStatusLive {
		parts = append(parts, "LIVE NOW")
	}
	// After LIVE NOW, because pinned is a fact about the live session and reads
	// as a qualifier on it rather than as a separate claim about the history.
	if row.Pinned {
		parts = append(parts, "PINNED")
	}
	if row.Hits > 0 {
		parts = append(parts, fmt.Sprintf("%d matching messages", row.Hits))
	}
	return strings.Join(parts, " · ")
}

func (a *app) writeConversationFooter(
	shown, matched, known int, query string, filters historyFilters, numbered, partial bool,
) error {
	headline := fmt.Sprintf("%d conversations match", matched)
	if matched == 1 {
		headline = "1 conversation matches"
	}
	if query != "" {
		headline += fmt.Sprintf(" %q", query)
	}
	if description := describeHistoryFilters(filters); description != "" {
		headline += " (" + description + ")"
	}
	parts := []string{headline}
	if shown < matched {
		parts = append(parts, fmt.Sprintf("showing the %d most recent, raise with -n", shown))
	}
	// "recorded" states a total, and a total is precisely what this number is
	// not when a machine is missing. Scoping the claim to the machines that
	// answered costs five words and keeps the line true.
	recorded := fmt.Sprintf("%d conversations recorded", known)
	if partial {
		recorded += " on the machines that answered"
	}
	parts = append(parts, recorded)
	if _, err := fmt.Fprintln(a.stdout, strings.Join(parts, " · ")); err != nil {
		return err
	}
	// The hint is for the reader who has not narrowed anything yet. Repeating
	// it after they have is noise, and noise under a list is what stops people
	// reading the honest counts above it.
	if query == "" && !filters.narrowed() && shown < matched {
		if _, err := fmt.Fprintln(a.stdout,
			"Narrow with a word from the conversation, --since today, --tool codex, or --cwd ."); err != nil {
			return err
		}
	}
	// --pick has to be discoverable without being compulsory. It is advertised
	// only to a person — both ends of this invocation are a terminal — and only
	// when they are not already using it, so a pipeline, a --json caller, and
	// every existing scripted reader of this output see byte-for-byte what they
	// saw before.
	if !numbered && a.attachedToTerminal() {
		_, err := fmt.Fprintln(a.stdout,
			"Reopen one without copying its command: add --pick.")
		return err
	}
	return nil
}

func (f historyFilters) narrowed() bool {
	return f.all || f.tool != "" || f.cwd != "" || f.nameGlob != "" ||
		len(f.sessions) > 0 || f.sinceMS != 0 || f.untilMS != 0 ||
		f.surface != "" || f.actor != ""
}

// emptyConversationAdvice never answers an empty browse with only "(none)".
// The reason a browse came back empty is nearly always a filter, and the user
// arrived here because they could not find a conversation in the first place.
func emptyConversationAdvice(known int, query string, filters historyFilters, partial bool) string {
	var builder strings.Builder
	builder.WriteString("(no conversations matched")
	if query != "" {
		fmt.Fprintf(&builder, " %q", query)
	}
	if description := describeHistoryFilters(filters); description != "" {
		builder.WriteString(" " + description)
	}
	builder.WriteString(")\n")
	where := ""
	if partial {
		where = " on the machines that answered"
	}
	if known > 0 {
		fmt.Fprintf(&builder,
			"%d conversations are recorded%s; widen the filters or run `sessions history --all`.\n", known, where)
		return builder.String()
	}
	builder.WriteString("No Claude or Codex history was found on the machines that answered.\n")
	return builder.String()
}

func describeHistoryFilters(filters historyFilters) string {
	parts := make([]string, 0, 5)
	if filters.tool != "" {
		parts = append(parts, "in "+filters.tool)
	}
	if filters.surface != "" {
		parts = append(parts, "started from "+filters.surface)
	}
	if filters.actor != "" {
		parts = append(parts, "driven by "+filters.actor)
	}
	if filters.sinceText != "" {
		parts = append(parts, "since "+filters.sinceText)
	}
	if filters.untilText != "" {
		parts = append(parts, "before "+filters.untilText)
	}
	if filters.cwd != "" {
		parts = append(parts, "under "+filters.cwd)
	}
	if filters.nameGlob != "" {
		parts = append(parts, "named "+filters.nameGlob)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func fleetHistoryTimedOut(target fleetTarget, budget time.Duration) error {
	if target.Endpoint == localFleetEndpoint {
		return fmt.Errorf("this machine did not answer within %s", localFleetRequestTimeout)
	}
	return fmt.Errorf(
		"did not answer within %s, so its conversations are missing; add --wait-for-peers, or run `sessions --machine %s history ...`",
		budget.Round(time.Millisecond), target.Alias,
	)
}

// fleetHistoryPeerTooSlow explains a peer this browse did not even ask. The
// distinction from an unreachable machine is the point: nothing is wrong with
// it, it is simply bigger than a browse, and the reader needs to know that the
// answer is one flag away rather than that their fleet is broken.
func fleetHistoryPeerTooSlow(target fleetTarget, listing fleetPeerListing) error {
	cost := (time.Duration(listing.TookMS) * time.Millisecond).Round(100 * time.Millisecond)
	// A completed listing measured the cost; an abandoned one only bounded it.
	// Printing a bound as a measurement would overstate what is known about the
	// machine, which is the mistake this whole change is about.
	observed := fmt.Sprintf("was still listing its history after %s last time", cost)
	if listing.Counted {
		observed = fmt.Sprintf("took %s to list its history last time", cost)
	}
	return fmt.Errorf(
		"not asked: it %s, longer than the %s a browse waits; add --wait-for-peers, or run `sessions --machine %s history ...`",
		observed, fleetHistoryPeerBudget, target.Alias,
	)
}

func fleetHistoryFailure(machines []historysearch.MachineState, rejection string) error {
	if rejection != "" {
		return fail(1, "%s", rejection)
	}
	lines := make([]string, 0, len(machines))
	for _, machine := range machines {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", machine.Alias, machine.Name, machine.Error))
	}
	if len(lines) == 0 {
		return fail(2, "no approved Sessions machine answered this history request")
	}
	return fail(2, "no approved Sessions machine answered this history request:\n%s", strings.Join(lines, "\n"))
}

// pluckOptionalCount reads a flag whose count may be omitted. --preview alone
// is the common case and has to stay one word; --preview 8 is the same option
// with the default overridden. A following token is only consumed when it is a
// bare number, so `--preview --tool codex` keeps its meaning.
func pluckOptionalCount(args *[]string, name string, fallback, maximum int) (int, bool, error) {
	for index, argument := range *args {
		if argument != name {
			continue
		}
		if index+1 < len(*args) {
			if parsed, err := strconv.Atoi((*args)[index+1]); err == nil {
				if parsed < 1 || parsed > maximum {
					return 0, false, fail(1, "%s must be between 1 and %d", name, maximum)
				}
				*args = append((*args)[:index], (*args)[index+2:]...)
				return parsed, true, nil
			}
		}
		*args = append((*args)[:index], (*args)[index+1:]...)
		return fallback, true, nil
	}
	return 0, false, nil
}

// parseHistoryTime accepts the way people say when. "today" is the question
// this command exists to answer, and refusing it because the daemon's search
// route only understands YYYY-MM-DD would reproduce the papercut. The second
// return value is how the answer will describe the bound back to the reader.
func parseHistoryTime(raw string, now time.Time, endOfDay bool) (int64, string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return 0, "", fail(1, "date filters need a value: today, yesterday, 3d, YYYY-MM-DD, or RFC3339")
	}
	startOfDay := func(day time.Time) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	}
	switch value {
	case "today":
		day := startOfDay(now)
		if endOfDay {
			day = day.AddDate(0, 0, 1)
		}
		return day.UnixMilli(), "today", nil
	case "yesterday":
		day := startOfDay(now).AddDate(0, 0, -1)
		if endOfDay {
			day = day.AddDate(0, 0, 1)
		}
		return day.UnixMilli(), "yesterday", nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UnixMilli(), parsed.Local().Format("2006-01-02 15:04"), nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		if endOfDay {
			parsed = parsed.AddDate(0, 0, 1)
		}
		return parsed.UnixMilli(), value, nil
	}
	if ago, ok := parseHistoryDuration(value); ok {
		return now.Add(-ago).UnixMilli(), value + " ago", nil
	}
	return 0, "", fail(1,
		"could not read the date %q; use today, yesterday, a span like 3d or 6h, YYYY-MM-DD, or RFC3339", raw)
}

// parseHistoryDuration extends Go's own units with the ones a person uses for
// conversation history. time.ParseDuration stops at hours, and "the last three
// days" is the most natural way to ask this question.
func parseHistoryDuration(value string) (time.Duration, bool) {
	if len(value) < 2 {
		return 0, false
	}
	unit := value[len(value)-1]
	amount, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || amount < 0 {
		if parsed, parseErr := time.ParseDuration(value); parseErr == nil && parsed >= 0 {
			return parsed, true
		}
		return 0, false
	}
	switch unit {
	case 'm':
		return time.Duration(amount) * time.Minute, true
	case 'h':
		return time.Duration(amount) * time.Hour, true
	case 'd':
		return time.Duration(amount) * 24 * time.Hour, true
	case 'w':
		return time.Duration(amount) * 7 * 24 * time.Hour, true
	}
	return 0, false
}

// historyToolName folds the provider spellings a stored conversation can carry
// into the three names the CLI filters on.
func historyToolName(tool string) string {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "claude", "claude-code":
		return "claude"
	case "codex":
		return "codex"
	case "terminal", "shell", "":
		return "shell"
	default:
		return strings.ToLower(strings.TrimSpace(tool))
	}
}

func (a *app) expandHome(path string) string {
	if path == "~" {
		return a.home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(a.home, rest)
	}
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func (a *app) shortenHome(path string) string {
	if a.home == "" {
		return path
	}
	if path == a.home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, a.home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// messageCountText never prints "0 messages" for a conversation nobody counted.
// Zero and unknown are different facts, and only one of them is a reason to
// pass a row by.
func (r conversationRow) messageCountText() string {
	if r.MessagesUncounted {
		return "messages not counted"
	}
	return pluralMessages(r.Messages)
}

func pluralMessages(count int) string {
	if count == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%s messages", groupThousands(count))
}

func groupThousands(value int) string {
	digits := strconv.Itoa(value)
	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	var builder strings.Builder
	for index, character := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			builder.WriteByte(',')
		}
		builder.WriteRune(character)
	}
	if negative {
		return "-" + builder.String()
	}
	return builder.String()
}

// firstSnippets keeps a browse readable. The evidence for a match is that it
// matched; two lines of it are enough to recognise the conversation, and
// `sessions search` remains the place to read every hit.
func firstSnippets(snippets []string) []string {
	if len(snippets) > historyMaxSnippets {
		return snippets[:historyMaxSnippets]
	}
	return snippets
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

const historyUsageText = "usage: sessions history [QUERY] [--since WHEN] [--until WHEN] [--tool claude|codex|shell] [--surface SURFACE] [--actor user|automation|agent] [--cwd PATH] [--name GLOB] [--session ID[,ID...]] [--touched] [--preview [N]] [--pick] [-n N] [--all] [--wait-for-peers] [--json]\nWHEN accepts today, yesterday, a span like 3d or 6h, YYYY-MM-DD, or RFC3339. SURFACE is codex-cli, codex-desktop, codex-exec, claude-cli, claude-desktop, claude-sdk, sessions, or the raw value a provider recorded. A QUERY, when given, comes FIRST, before any flags"
