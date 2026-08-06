package main

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

// The browser exists because neither provider can answer "which conversation
// was that". `claude --resume` is scoped to the directory you happen to be
// standing in and its git worktrees, and Codex's own picker filters by cwd
// too, so a conversation you started somewhere else is unreachable unless you
// first remember where you started it. Sessions already resolves conversation
// ids fleet-wide; this command is the missing view over that: every recorded
// Claude and Codex conversation, newest first, each row carrying enough to
// recognise it and the exact command that brings it back from any directory.
const (
	// A history listing parses every transcript it counts, so it costs more on
	// the answering daemon than a search does. Peers therefore get a larger
	// wall-clock budget than fleetPeerBudget allows a search — and still a
	// bounded one, because the local machine owns the answer and one slow peer
	// may not delay it.
	fleetHistoryPeerBudget = 2 * time.Second
	// Rows shown by default. A browser that prints every one of several
	// hundred conversations has not helped anyone find one; the count that
	// matched is always reported, so the cut is visible rather than silent.
	historyDefaultRows = 20
	// Previews are several lines each, so asking for them narrows the page
	// unless the caller said otherwise.
	historyPreviewRows        = 5
	historyDefaultPreview     = 4
	historyMaxRows            = 500
	historyMaxPreview         = 20
	historyPreviewConcurrency = 6
	historySnippetRunes       = 150
	historyMaxSnippets        = 2
)

// Statuses a conversation row can report. They name why a row does or does not
// carry a command, and they are the same words in the table and in --json.
const (
	historyStatusResumable     = "resumable"
	historyStatusLive          = "live"
	historyStatusMoved         = "moved"
	historyStatusUnreadable    = "unreadable"
	historyStatusUnrecoverable = "unrecoverable"
)

// conversationRow is one recorded conversation as the browser sees it. Status,
// reason and resume follow `sessions recover`'s discipline deliberately: the
// command printed against a row is the one that actually works for that row,
// and a row nothing can bring back carries no command at all rather than a
// plausible-looking one that would be refused.
type conversationRow struct {
	Reference string `json:"reference"`
	ID        string `json:"id"`
	// Machine is the approved-fleet alias the conversation was found on.
	Machine  string `json:"machine"`
	Name     string `json:"name"`
	Tool     string `json:"tool"`
	CWD      string `json:"cwd"`
	Messages int    `json:"messages"`
	// LastActiveAt and LastActiveAtMS are the same instant twice: the string is
	// what a human reads back, the milliseconds are what a caller sorts on.
	LastActiveAt   string   `json:"last_active_at,omitempty"`
	LastActiveAtMS int64    `json:"last_active_at_ms"`
	Status         string   `json:"status"`
	Reason         string   `json:"reason,omitempty"`
	Resume         []string `json:"resume,omitempty"`
	// Hits and Snippets are only present when a query narrowed the browse, and
	// say why this conversation is in the answer.
	Hits     int      `json:"hits,omitempty"`
	Snippets []string `json:"snippets,omitempty"`
	// Preview is the tail of the conversation, present only under --preview.
	Preview []conversationPreviewMessage `json:"preview,omitempty"`
	// PreviewError explains a preview that could not be read. A row whose
	// preview failed is still a row: losing the tail must not lose the
	// conversation from the list.
	PreviewError string `json:"preview_error,omitempty"`

	target int
}

type conversationPreviewMessage struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

type historyBrowseResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Query         string `json:"query,omitempty"`
	// Known counts every conversation the answering machines listed, Matched
	// counts the ones that survived the filters, and Shown counts the ones on
	// this page. Three numbers because a browser that silently truncates is
	// indistinguishable from one that found nothing else.
	Known         int                          `json:"known"`
	Matched       int                          `json:"matched"`
	Shown         int                          `json:"shown"`
	Conversations []conversationRow            `json:"conversations"`
	Machines      []historysearch.MachineState `json:"machines,omitempty"`
	Partial       bool                         `json:"partial,omitempty"`
}

type historyFilters struct {
	tool      string
	cwd       string
	nameGlob  string
	sessions  []string
	sinceMS   int64
	untilMS   int64
	sinceText string
	untilText string
	all       bool
	explicit  bool // a tool was named, so the conversation-only default is off
}

func (a *app) cmdHistory(args []string) error {
	query := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query = strings.TrimSpace(args[0])
		args = args[1:]
	}
	all := removeFirst(&args, "--all")
	previewCount, wantPreview, err := pluckOptionalCount(&args, "--preview", historyDefaultPreview, historyMaxPreview)
	if err != nil {
		return err
	}
	tool, hasTool := pluck(&args, "--tool")
	cwd, hasCWD := pluck(&args, "--cwd")
	name, hasName := pluck(&args, "--name")
	lane, hasLane := pluck(&args, "--lane")
	since, hasSince := pluck(&args, "--since")
	until, hasUntil := pluck(&args, "--until")
	sessionIDs, hasSession := pluck(&args, "--session")
	limitText, hasLimit := pluck(&args, "-n")
	if len(args) != 0 {
		return fail(1, "unknown history option: %s\n%s", args[0], historyUsageText)
	}
	if hasName && hasLane {
		return fail(1, "--name and --lane are aliases; use only one")
	}
	if hasLane {
		name, hasName = lane, true
	}

	filters := historyFilters{all: all}
	if hasTool {
		filters.tool = strings.ToLower(strings.TrimSpace(tool))
		if filters.tool != "claude" && filters.tool != "codex" && filters.tool != "shell" {
			return fail(1, "--tool must be \"claude\", \"codex\", or \"shell\"")
		}
		filters.explicit = true
	}
	if hasCWD {
		filters.cwd = a.expandHome(strings.TrimSpace(cwd))
		if filters.cwd == "" {
			return fail(1, "--cwd needs a path")
		}
	}
	if hasName {
		filters.nameGlob = strings.TrimSpace(name)
		if _, matchErr := filepath.Match(filters.nameGlob, "conversation"); matchErr != nil {
			return fail(1, "invalid --name glob: %s", matchErr)
		}
	}
	if hasSession {
		for _, value := range strings.Split(sessionIDs, ",") {
			if value = strings.TrimSpace(value); value != "" {
				filters.sessions = append(filters.sessions, value)
			}
		}
		if len(filters.sessions) == 0 {
			return fail(1, "--session needs a conversation id")
		}
	}
	if hasSince {
		value, text, timeErr := parseHistoryTime(since, a.now(), false)
		if timeErr != nil {
			return timeErr
		}
		filters.sinceMS, filters.sinceText = value, text
	}
	if hasUntil {
		value, text, timeErr := parseHistoryTime(until, a.now(), true)
		if timeErr != nil {
			return timeErr
		}
		filters.untilMS, filters.untilText = value, text
	}
	if filters.sinceMS != 0 && filters.untilMS != 0 && filters.sinceMS >= filters.untilMS {
		return fail(1, "--since must be before --until")
	}
	limit := historyDefaultRows
	if wantPreview {
		limit = historyPreviewRows
	}
	if hasLimit {
		parsed, convErr := strconv.Atoi(limitText)
		if convErr != nil || parsed < 1 || parsed > historyMaxRows {
			return fail(1, "-n must be between 1 and %d", historyMaxRows)
		}
		limit = parsed
	}

	targets, err := a.historyTargets()
	if err != nil {
		return err
	}
	defer closeFleetTargets(targets)

	collected, err := a.collectConversations(targets)
	if err != nil {
		return err
	}
	rows := collected.rows
	if query != "" {
		rows, err = a.narrowConversationsByQuery(rows, query, filters)
		if err != nil {
			return err
		}
	}
	rows = filterConversations(rows, filters)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].LastActiveAtMS != rows[j].LastActiveAtMS {
			return rows[i].LastActiveAtMS > rows[j].LastActiveAtMS
		}
		return rows[i].Reference < rows[j].Reference
	})
	matched := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if wantPreview {
		a.attachConversationPreviews(rows, targets, previewCount)
	}

	if a.wantJSON {
		return writeJSON(a.stdout, historyBrowseResponse{
			SchemaVersion: integrations.SchemaVersion, Query: query,
			Known: collected.known, Matched: matched, Shown: len(rows),
			Conversations: rows, Machines: collected.machines, Partial: collected.partial,
		}, true)
	}
	if collected.partial {
		for _, machine := range collected.machines {
			if machine.Status == "unavailable" {
				fmt.Fprintf(a.stderr, "sessions: %s was unavailable: %s\n", machine.Name, machine.Error)
			}
		}
	}
	return a.writeConversationRows(rows, matched, collected.known, query, filters)
}

// historyTargets is the approved fleet unless the caller pinned one daemon.
// Browsing has to reach every machine a conversation could be on for the same
// reason search does: the whole point is not having to remember where you were.
func (a *app) historyTargets() ([]fleetTarget, error) {
	if a.explicitTarget {
		return []fleetTarget{{
			Alias: "local", Name: "This machine", Endpoint: localFleetEndpoint, Client: a.api,
		}}, nil
	}
	targets, err := a.approvedFleetTargets()
	if err != nil {
		return nil, fail(2, "read approved machines: %s", err)
	}
	return targets, nil
}

func closeFleetTargets(targets []fleetTarget) {
	for _, target := range targets {
		if target.Owned {
			target.Client.close()
		}
	}
}

// collectedConversations is one fleet-wide listing: the rows themselves, which
// machines were reached, and how many conversations exist before any filter.
type collectedConversations struct {
	rows     []conversationRow
	machines []historysearch.MachineState
	partial  bool
	known    int
}

type historyTargetOutcome struct {
	index   int
	listing integrations.HistoryResponse
	live    map[string]bool
	err     error
}

// collectConversations asks every target for its conversations and for the
// sessions it currently has running. Both come from the same daemon on
// purpose: a conversation that is live right now is not resumable — Sessions'
// own guard refuses it and tells you to attach instead — so a browser that
// could not tell the difference would print a command that fails on exactly
// the conversation the user is most likely to pick.
func (a *app) collectConversations(targets []fleetTarget) (collectedConversations, error) {
	health := readFleetPeerHealth(a.home)
	now := a.now()
	outcomes := make([]historyTargetOutcome, len(targets))
	answers := make(chan historyTargetOutcome, len(targets))
	dispatched := make([]bool, len(targets))
	pending := 0
	awaitingLocal := false
	for index := range targets {
		target := targets[index]
		if target.Endpoint != localFleetEndpoint {
			if failure, retryAt, cooling := health.coolingDown(target.Alias, now); cooling {
				outcomes[index].err = fleetPeerSkipped(target, "history", failure, retryAt.Sub(now))
				continue
			}
		}
		outcomes[index] = historyTargetOutcome{index: index, err: fleetHistoryTimedOut(target)}
		dispatched[index] = true
		pending++
		awaitingLocal = awaitingLocal || target.Endpoint == localFleetEndpoint
		go func(index int) {
			answers <- readTargetConversations(targets[index], index)
		}(index)
	}

	// The local machine owns the answer, so it is always awaited; peers only
	// add to it and are dropped once the budget passes.
	budget := time.NewTimer(fleetHistoryPeerBudget)
	defer budget.Stop()
	budgetExpired := budget.C
	expired := false
	accept := func(outcome historyTargetOutcome) {
		outcomes[outcome.index] = outcome
		pending--
		if targets[outcome.index].Endpoint == localFleetEndpoint {
			awaitingLocal = false
		}
	}
	for pending > 0 {
		select {
		case outcome := <-answers:
			accept(outcome)
			continue
		default:
		}
		if expired && !awaitingLocal {
			break
		}
		select {
		case outcome := <-answers:
			accept(outcome)
		case <-budgetExpired:
			expired = true
			budgetExpired = nil
		}
	}

	qualify := !a.explicitTarget && len(targets) > 1
	collected := collectedConversations{
		rows:     make([]conversationRow, 0, 64),
		machines: make([]historysearch.MachineState, 0, len(targets)),
	}
	successes := 0
	rejection := ""
	for index, target := range targets {
		outcome := outcomes[index]
		state := historysearch.MachineState{
			Alias: target.Alias, Name: target.Name, Endpoint: target.Endpoint, Status: "listed",
		}
		if outcome.err != nil {
			state.Status = "unavailable"
			state.Error = outcome.err.Error()
			collected.partial = true
			collected.machines = append(collected.machines, state)
			if refusal, refused := requestWasRejected(outcome.err); refused {
				if rejection == "" {
					rejection = refusal.Error()
				}
				health.recordSuccess(target.Alias)
			} else if dispatched[index] && target.Endpoint != localFleetEndpoint {
				health.recordFailure(target.Alias, now, outcome.err)
			}
			continue
		}
		successes++
		health.recordSuccess(target.Alias)
		collected.machines = append(collected.machines, state)
		collected.known += len(outcome.listing.Sessions)
		for _, session := range outcome.listing.Sessions {
			collected.rows = append(collected.rows, conversationRow{
				target: index,
			}.fill(target.Alias, qualify, session, outcome.live[session.ID]))
		}
	}
	health.save(a.home)
	if successes == 0 {
		return collectedConversations{}, fleetHistoryFailure(collected.machines, rejection)
	}
	return collected, nil
}

func readTargetConversations(target fleetTarget, index int) historyTargetOutcome {
	outcome := historyTargetOutcome{index: index, live: map[string]bool{}}
	timeout := fleetTargetTimeout(target)
	// The running set is read first because it is the cheap call: if this
	// daemon cannot answer at all, nothing below is worth attempting, and a
	// listing without it would misreport which conversations can be resumed.
	var running sessionsResponse
	if outcome.err = getJSONFromClient(target.Client, "/api/sessions", &running, timeout); outcome.err != nil {
		return outcome
	}
	for _, value := range running.Sessions {
		if !value.Exited {
			outcome.live[value.ID] = true
		}
	}
	outcome.err = getJSONFromClient(target.Client, "/api/history", &outcome.listing, timeout)
	return outcome
}

// fill turns one stored conversation into a browsable row, including the
// verdict on how it comes back.
func (r conversationRow) fill(
	alias string, qualify bool, session integrations.HistorySession, live bool,
) conversationRow {
	reference := session.ID
	if qualify {
		reference = qualifiedHistoryReference(alias, session.ID)
	}
	r.Reference = reference
	r.ID = session.ID
	r.Machine = alias
	r.Name = strings.TrimSpace(session.Name)
	r.Tool = historyToolName(session.Tool)
	r.CWD = session.CWD
	r.Messages = session.MessageCount
	// When the conversation was last written to is the question a browser is
	// ordering by, and it is not the same as when the Sessions record was last
	// touched. A shutdown sweep that drains sixteen finished runners moves
	// every one of their record timestamps to the same instant; the transcripts
	// they name did not change, and it is the transcripts the user remembers.
	r.LastActiveAtMS = session.ConversationUpdatedAt
	if r.LastActiveAtMS == 0 {
		r.LastActiveAtMS = session.LastActivityAt
	}
	if r.LastActiveAtMS > 0 {
		r.LastActiveAt = time.UnixMilli(r.LastActiveAtMS).Format(time.RFC3339)
	}
	r.Status, r.Reason, r.Resume = conversationRecovery(session, reference, live)
	return r
}

// conversationRecovery decides what a row can be told to do. The rule is the
// one `sessions recover` holds itself to: print the command that works, or
// print none and say why. `sessions resume` is the right command even for a
// conversation whose provider deleted its own transcript, because resume
// replays Sessions' own copy for exactly that case — which is also why the
// provider's native resume flag is never printed here.
func conversationRecovery(
	session integrations.HistorySession, reference string, live bool,
) (string, string, []string) {
	if live {
		return historyStatusLive,
			"still running; attach instead of resuming",
			[]string{"sessions", "attach", prefixString(session.ID, 8)}
	}
	if session.MovedToEndpoint != "" {
		return historyStatusMoved,
			"continued on " + session.MovedToEndpoint + "; resume it there",
			nil
	}
	if session.Unreadable {
		reason := strings.TrimSpace(session.UnreadableReason)
		if reason == "" {
			reason = "this conversation could not be read on this pass"
		}
		return historyStatusUnreadable, reason, nil
	}
	if !session.ConversationAvailable {
		return historyStatusUnrecoverable,
			"neither the provider nor Sessions still holds this conversation",
			nil
	}
	return historyStatusResumable, "", []string{"sessions", "resume", reference}
}

// narrowConversationsByQuery keeps only the conversations whose text matched,
// using search's own per-session rollup. That rollup is computed over every
// hit rather than over a page of messages, which is why it can answer "which
// conversation" at all — and until now the CLI computed it, merged it across
// the fleet, and printed nothing from it.
func (a *app) narrowConversationsByQuery(
	rows []conversationRow, query string, filters historyFilters,
) ([]conversationRow, error) {
	parameters := url.Values{"q": {query}, "ranked": {"true"}}
	if filters.tool != "" {
		parameters.Set("tool", filters.tool)
	}
	if filters.cwd != "" {
		parameters.Set("cwd", filters.cwd)
	}
	if filters.nameGlob != "" {
		parameters.Set("name", filters.nameGlob)
	}
	if filters.sinceMS != 0 {
		parameters.Set("since", time.UnixMilli(filters.sinceMS).Format(time.RFC3339))
	}
	if filters.untilMS != 0 {
		parameters.Set("until", time.UnixMilli(filters.untilMS).Format(time.RFC3339))
	}
	path := "/api/search?" + parameters.Encode()
	var result historysearch.Response
	if a.explicitTarget {
		if err := a.searchOneDaemon(path, &result); err != nil {
			return nil, err
		}
	} else {
		var err error
		result, err = a.searchApprovedFleet(path, 0, false, false, true)
		if err != nil {
			return nil, err
		}
	}
	hits := conversationsBehindMatches(result)
	kept := rows[:0]
	for _, row := range rows {
		evidence, matched := hits[conversationKey(row.Machine, row.ID)]
		if !matched {
			continue
		}
		row.Hits = evidence.hits
		row.Snippets = evidence.snippets
		kept = append(kept, row)
	}
	return kept, nil
}

// conversationEvidence is why one conversation is in a narrowed answer. hits is
// left at zero unless the rollup supplied it, because a count derived from a
// page of messages is a lower bound and printing it as a total would be a
// quiet lie about how much of the conversation is about this.
type conversationEvidence struct {
	hits     int
	snippets []string
}

// conversationsBehindMatches folds a search answer down to the conversations it
// implicates. The per-session rollup is the better source — it is computed over
// every hit rather than over one page — but it is capped at fifty sessions and
// older daemons do not send it at all, and a conversation that reached the
// message page while missing from the rollup is still an answer. Reading both
// is what keeps this from silently reporting "no conversations matched" for a
// search that plainly did match.
func conversationsBehindMatches(result historysearch.Response) map[string]conversationEvidence {
	evidence := make(map[string]conversationEvidence, len(result.Sessions)+len(result.Matches))
	for _, rollup := range result.Sessions {
		evidence[conversationKey(rollup.Machine, rollup.SessionID)] = conversationEvidence{
			hits: rollup.Hits, snippets: rollup.Snippets,
		}
	}
	for _, match := range result.Matches {
		key := conversationKey(match.MachineAlias, match.SessionID)
		found := evidence[key]
		if len(found.snippets) < historyMaxSnippets && strings.TrimSpace(match.Snippet) != "" {
			found.snippets = append(found.snippets, match.Snippet)
		}
		evidence[key] = found
	}
	return evidence
}

// conversationKey identifies one conversation on one machine. Search qualifies
// its rollup ids in fleet mode and leaves them bare against a single daemon, so
// both spellings have to collapse to the same key.
func conversationKey(machine, sessionID string) string {
	if alias, id, qualified := splitQualifiedHistoryReference(sessionID); qualified {
		return alias + "\x00" + id
	}
	if machine == "" {
		machine = "local"
	}
	return machine + "\x00" + sessionID
}

// filterConversations applies the narrowing the caller asked for, plus the one
// it did not: by default a browse answers with conversations, not with every
// record the daemon keeps. Empty shells, shell lanes and conversations nothing
// can read are the rows a person scrolls past, so they are behind --all — and
// the count of what was filtered is always printed, so the cut is never silent.
func filterConversations(rows []conversationRow, filters historyFilters) []conversationRow {
	kept := make([]conversationRow, 0, len(rows))
	for _, row := range rows {
		if filters.tool != "" && row.Tool != filters.tool {
			continue
		}
		if !filters.all && !filters.explicit {
			if row.Tool != "claude" && row.Tool != "codex" {
				continue
			}
		}
		if !filters.all {
			if row.Messages <= 0 || row.Status == historyStatusUnrecoverable || row.Status == historyStatusUnreadable {
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
	return kept
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

func (a *app) writeConversationRows(
	rows []conversationRow, matched, known int, query string, filters historyFilters,
) error {
	if len(rows) == 0 {
		_, err := io.WriteString(a.stdout, emptyConversationAdvice(known, query, filters))
		return err
	}
	for _, row := range rows {
		name := row.Name
		if name == "" {
			name = "(unnamed)"
		}
		if _, err := fmt.Fprintf(a.stdout, "%s\n", name); err != nil {
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
	return a.writeConversationFooter(len(rows), matched, known, query, filters)
}

func (a *app) conversationMetaLine(row conversationRow) string {
	parts := make([]string, 0, 6)
	when := "no recorded activity"
	if row.LastActiveAtMS > 0 {
		when = fmt.Sprintf("%s · %s ago",
			time.UnixMilli(row.LastActiveAtMS).Format("2006-01-02 15:04"), a.ageOf(row.LastActiveAtMS))
	}
	parts = append(parts, when, row.Tool, pluralMessages(row.Messages))
	if row.CWD != "" {
		parts = append(parts, a.shortenHome(row.CWD))
	}
	if row.Machine != "" && row.Machine != "local" {
		parts = append(parts, "on "+row.Machine)
	}
	if row.Status == historyStatusLive {
		parts = append(parts, "LIVE NOW")
	}
	if row.Hits > 0 {
		parts = append(parts, fmt.Sprintf("%d matching messages", row.Hits))
	}
	return strings.Join(parts, " · ")
}

func (a *app) writeConversationFooter(
	shown, matched, known int, query string, filters historyFilters,
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
	parts = append(parts, fmt.Sprintf("%d conversations recorded", known))
	if _, err := fmt.Fprintln(a.stdout, strings.Join(parts, " · ")); err != nil {
		return err
	}
	// The hint is for the reader who has not narrowed anything yet. Repeating
	// it after they have is noise, and noise under a list is what stops people
	// reading the honest counts above it.
	if query == "" && !filters.narrowed() && shown < matched {
		_, err := fmt.Fprintln(a.stdout,
			"Narrow with a word from the conversation, --since today, --tool codex, or --cwd .")
		return err
	}
	return nil
}

func (f historyFilters) narrowed() bool {
	return f.all || f.tool != "" || f.cwd != "" || f.nameGlob != "" ||
		len(f.sessions) > 0 || f.sinceMS != 0 || f.untilMS != 0
}

// emptyConversationAdvice never answers an empty browse with only "(none)".
// The reason a browse came back empty is nearly always a filter, and the user
// arrived here because they could not find a conversation in the first place.
func emptyConversationAdvice(known int, query string, filters historyFilters) string {
	var builder strings.Builder
	builder.WriteString("(no conversations matched")
	if query != "" {
		fmt.Fprintf(&builder, " %q", query)
	}
	if description := describeHistoryFilters(filters); description != "" {
		builder.WriteString(" " + description)
	}
	builder.WriteString(")\n")
	if known > 0 {
		fmt.Fprintf(&builder, "%d conversations are recorded; widen the filters or run `sessions history --all`.\n", known)
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

func fleetHistoryTimedOut(target fleetTarget) error {
	if target.Endpoint == localFleetEndpoint {
		return fmt.Errorf("this machine did not answer within %s", localFleetRequestTimeout)
	}
	return fmt.Errorf(
		"did not answer within %s, so its conversations are missing; run `sessions --machine %s history ...` to wait for it",
		fleetHistoryPeerBudget, target.Alias,
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

const historyUsageText = "usage: sessions history [QUERY] [--since WHEN] [--until WHEN] [--tool claude|codex|shell] [--cwd PATH] [--name GLOB] [--session ID[,ID...]] [--preview [N]] [-n N] [--all] [--json]\nWHEN accepts today, yesterday, a span like 3d or 6h, YYYY-MM-DD, or RFC3339. A QUERY, when given, comes FIRST, before any flags"
