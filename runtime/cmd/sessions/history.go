package main

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
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
	//
	// Two seconds was measured against a two-machine fleet whose peer held 1519
	// of 1825 recorded conversations and answered in 3.3-3.9 seconds across
	// nineteen consecutive tries. It missed every one, so the browse paid the
	// full two seconds and returned 17% of the fleet: the worst value a budget
	// can take, long enough to be felt and short enough to always fail.
	//
	// Raising it does not fix that, and the attempt is instructive. Granting the
	// peer the time it had proven it needed produced browses of three, four and
	// a half, then five seconds that still did not reach it, because a peer's
	// cost grows with the history it accumulates and the honest number is simply
	// larger than a browse can spend. Sessions cannot make a remote machine
	// answer quickly, so a budget wide enough to guarantee the fleet is a
	// guarantee it cannot keep at a price the command can afford.
	//
	// What it can do is stop paying for the failure. The budget stays at the
	// point where a browse still feels immediate, a peer that has shown it
	// cannot answer inside it is left out at no cost rather than waited for at
	// full cost, what it holds is stated where the counts are, and
	// --wait-for-peers buys the complete answer for anyone who wants it.
	fleetHistoryPeerBudget = 2 * time.Second
	// How long a peer that has shown it cannot answer inside the budget is taken
	// at its word before being tried again. A machine gets faster, or its
	// history gets smaller, or the network stops being terrible; none of those
	// are events Sessions is told about, so it re-checks on its own rather than
	// writing the peer off until something clears a cache.
	fleetHistoryPeerRecheck = 10 * time.Minute
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
	// PromptHistoryOnly marks a row recovered from Claude's prompt archive
	// rather than from a transcript. That archive keeps the user's prompts and
	// nothing else, so it can never say where a conversation was started — which
	// is a different fact from a daemon that did not look.
	PromptHistoryOnly bool `json:"prompt_history_only,omitempty"`
	// MessagesUncounted marks a Messages that is not a count, because the
	// answering daemon declined to parse that transcript. An unknown count must
	// not be read as an empty conversation: that mistake hides exactly the rows
	// a browse exists to show.
	MessagesUncounted bool `json:"messages_uncounted,omitempty"`
	// Surface is where the conversation was started from and who drove it — the
	// thing neither provider's own picker will tell you. SurfaceKind is the
	// token --surface matches, Surface is what a person reads, SurfaceRaw is
	// exactly what the provider wrote. All are empty when the answering daemon
	// did not report any of it, which is not the same as "started nowhere".
	Surface     string `json:"surface,omitempty"`
	SurfaceKind string `json:"surface_kind,omitempty"`
	SurfaceRaw  string `json:"surface_raw,omitempty"`
	Actor       string `json:"actor,omitempty"`
	// ApproximateTime marks a row whose last-active time is the transcript
	// file's modification time rather than the conversation's own last record.
	// A history copied without preserving times reports every conversation as
	// new, and a row that cannot say so is misleading in the one column the
	// whole list is sorted by.
	ApproximateTime bool `json:"approximate_time,omitempty"`
	// LastActiveAt and LastActiveAtMS are the same instant twice: the string is
	// what a human reads back, the milliseconds are what a caller sorts on.
	LastActiveAt   string `json:"last_active_at,omitempty"`
	LastActiveAtMS int64  `json:"last_active_at_ms"`
	// StartedAt and StartedAtMS are when the conversation began, which is not
	// recoverable from anything else on the row. A Codex rollout is filed under
	// the date it started and may still be written to weeks later, so the only
	// two questions a person asks about a half-remembered session -- when did I
	// start this, and when did I last touch it -- need both numbers.
	StartedAt   string   `json:"started_at,omitempty"`
	StartedAtMS int64    `json:"started_at_ms,omitempty"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Resume      []string `json:"resume,omitempty"`
	// Pinned marks a conversation whose live Sessions record the user pinned.
	// It is the answer to "which of these is one of mine" in a browse that is
	// deliberately fleet-wide and mostly other people's and other days' work.
	// A conversation with no live session cannot be pinned, so this is absent
	// rather than false on every historical row.
	Pinned bool `json:"pinned,omitempty"`
	// LastHumanMessageAtMS is when a person last spoke into this conversation
	// through Sessions, which is a different fact from LastActiveAtMS: a lane
	// driven entirely by its own scheduled prompts is active every half hour and
	// has never been spoken to. It is present only for a conversation with a
	// live session, because the daemon stamps it at its input boundary and has
	// nowhere to keep it once the session is gone. Absent therefore means "not
	// known here", not "never".
	LastHumanMessageAtMS int64 `json:"last_human_message_at_ms,omitempty"`
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
	// Withheld is what the machines that did not answer are known to hold. A
	// caller that read Known alone would report the machines that answered as
	// the fleet; this is the correction, and it is present exactly when Partial
	// is true.
	Withheld []withheldMachine `json:"withheld,omitempty"`
	// ProvenanceUnreported counts conversations excluded by --surface or
	// --actor because the daemon that listed them reported no provenance at
	// all. Nonzero means the answer is short by that many rows for a reason
	// that has nothing to do with the filter.
	ProvenanceUnreported int `json:"provenance_unreported,omitempty"`
}

type historyFilters struct {
	tool        string
	cwd         string
	nameGlob    string
	sessions    []string
	sinceMS     int64
	untilMS     int64
	sinceText   string
	untilText   string
	surface     string
	surfaceText string
	actor       string
	all         bool
	// touched keeps only conversations a person actually spoke into.
	touched  bool
	explicit bool // a tool was named, so the conversation-only default is off
}

// wantsProvenance reports whether the caller narrowed on something only a
// daemon new enough to report a surface can answer.
func (f historyFilters) wantsProvenance() bool {
	return f.surface != "" || f.actor != ""
}

func (a *app) cmdHistory(args []string) error {
	query := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query = strings.TrimSpace(args[0])
		args = args[1:]
	}
	all := removeFirst(&args, "--all")
	touched := removeFirst(&args, "--touched")
	pick := removeFirst(&args, "--pick")
	waitForPeers := removeFirst(&args, "--wait-for-peers")
	previewCount, wantPreview, err := pluckOptionalCount(&args, "--preview", historyDefaultPreview, historyMaxPreview)
	if err != nil {
		return err
	}
	tool, hasTool := pluck(&args, "--tool")
	surface, hasSurface := pluck(&args, "--surface")
	actor, hasActor := pluck(&args, "--actor")
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
	// --pick is the only thing that makes this command interactive, and it is
	// refused rather than ignored next to --json. A caller that asked for one
	// JSON document must never be handed a prompt it will not answer and a
	// stream that never ends; silently dropping the flag instead would leave
	// them believing they had a picker.
	if pick && a.wantJSON {
		return fail(1, "--pick is interactive; it cannot be combined with --json")
	}
	if hasLane {
		name, hasName = lane, true
	}

	filters := historyFilters{all: all, touched: touched}
	if hasTool {
		filters.tool = strings.ToLower(strings.TrimSpace(tool))
		if filters.tool != "claude" && filters.tool != "codex" && filters.tool != "shell" {
			return fail(1, "--tool must be \"claude\", \"codex\", or \"shell\"")
		}
		filters.explicit = true
	}
	if hasSurface {
		filters.surfaceText = strings.TrimSpace(surface)
		filters.surface = watch.NormalizeSurfaceKind(filters.surfaceText)
		if filters.surface == "" {
			return fail(1, "--surface needs a surface: %s, or the raw value a provider recorded",
				strings.Join(watch.KnownSurfaceKinds(), ", "))
		}
	}
	if hasActor {
		filters.actor = watch.NormalizeActor(actor)
		if filters.actor == "" {
			return fail(1, "--actor must be \"user\", \"automation\", or \"agent\"")
		}
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

	collected, err := a.collectConversations(targets, waitForPeers)
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
	candidates := rows
	// Which machines answered with any provenance at all is decided over every
	// candidate, before the page limit, so a machine whose only matching rows
	// fell off the page is not accused of being unable to answer.
	answered := machinesReportingProvenance(candidates)
	rows, withoutProvenance := filterConversations(rows, filters)
	sort.SliceStable(rows, func(i, j int) bool {
		// Under --touched the question is "what did I last speak to", so the
		// order is human recency. Ordering those rows by transcript activity
		// would put the lane whose own cron fired a minute ago above the one the
		// user was actually talking to an hour ago, which is the confusion the
		// filter exists to end.
		if filters.touched {
			if rows[i].LastHumanMessageAtMS != rows[j].LastHumanMessageAtMS {
				return rows[i].LastHumanMessageAtMS > rows[j].LastHumanMessageAtMS
			}
			return rows[i].Reference < rows[j].Reference
		}
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
			Withheld:             collected.withheld,
			ProvenanceUnreported: len(withoutProvenance),
		}, true)
	}
	if collected.partial {
		for _, machine := range collected.machines {
			if machine.Status == "unavailable" {
				fmt.Fprintf(a.stderr, "sessions: %s was unavailable: %s\n", machine.Name, machine.Error)
			}
		}
	}
	// The listing is drawn through a closure so the picker can redraw exactly
	// the page it is picking from after a preview has scrolled it away.
	render := func() error {
		return a.writeConversationRows(
			rows, matched, collected.known, query, filters, withoutProvenance, answered, pick,
			collected.withheld, collected.waited)
	}
	if err := render(); err != nil {
		return err
	}
	if err := a.writeSurfacesSeen(candidates, matched, filters); err != nil {
		return err
	}
	if !pick {
		return nil
	}
	return a.pickConversation(rows, targets, render)
}

// writeSurfacesSeen answers a --surface that matched nothing with the surfaces
// that are actually there.
//
// --surface deliberately accepts more than the curated tokens: a provider can
// add an originator tomorrow, and a browse that refused the new value would be
// unable to reach exactly the conversations a user is most confused about. The
// cost of accepting anything is that a typo comes back as an empty list, so an
// empty list says what could have been typed instead — read off this machine's
// own history rather than from a hardcoded vocabulary.
func (a *app) writeSurfacesSeen(candidates []conversationRow, matched int, filters historyFilters) error {
	if matched > 0 || filters.surface == "" {
		return nil
	}
	seen := make(map[string]struct{}, 8)
	available := make([]string, 0, 8)
	for _, row := range candidates {
		for _, value := range []string{row.SurfaceKind, row.SurfaceRaw} {
			if value == "" {
				continue
			}
			if _, known := seen[value]; known {
				continue
			}
			seen[value] = struct{}{}
			available = append(available, value)
		}
	}
	if len(available) == 0 {
		return nil
	}
	sort.Strings(available)
	_, err := fmt.Fprintf(a.stdout,
		"No conversation was started from %q. Surfaces recorded here: %s.\n",
		filters.surfaceText, strings.Join(available, ", "))
	return err
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
	// withheld is one entry per machine that did not answer. known counts the
	// conversations on the machines that did, so on its own it describes a
	// fraction of the fleet as though it were the whole of it. These entries are
	// what let the answer say which fraction.
	withheld []withheldMachine
	// waited records that this browse already spent everything it had on the
	// peers. A shortfall line that recommends --wait-for-peers to a caller who
	// used --wait-for-peers is worse than no advice: it sends them back around a
	// loop they have already run.
	waited bool
}

// withheldMachine is a machine missing from an answer, and the scale of what it
// took with it. Conversations is what that machine held the last time this one
// reached it; Counted is false when it has never been reached from here, which
// is a different and more alarming fact than holding nothing.
type withheldMachine struct {
	Alias         string `json:"alias"`
	Name          string `json:"name"`
	Conversations int    `json:"conversations,omitempty"`
	Counted       bool   `json:"counted"`
	CountedAt     string `json:"counted_at,omitempty"`
	Reason        string `json:"reason"`

	countedAtMS int64
}

type historyTargetOutcome struct {
	index   int
	listing integrations.HistoryResponse
	live    map[string]bool
	// pinned is read from the same listing as live, because a pin is a fact
	// about a running session and only a running session can carry one.
	pinned map[string]bool
	// humanAt is when a person last spoke into the live session, read from the
	// same listing for the same reason: the daemon stamps it at its own input
	// boundary, so only a session it still holds can answer.
	humanAt map[string]int64
	took    time.Duration
	err     error
}

// peerCannotAnswerInTime reports a peer that has already shown it needs longer
// than a browse waits, and is therefore left out of one instead of stalling it.
//
// This is the whole difference between the two seconds a browse used to spend
// discovering the same thing every time and the nothing it spends now. It is a
// claim about the past, checked again on a timer, not a verdict: the peer is
// re-tried once the recheck window passes, and a --wait-for-peers browse ignores
// it entirely.
func peerCannotAnswerInTime(
	health *fleetPeerHealth, alias string, now time.Time,
) (fleetPeerListing, bool) {
	listing, _, known := health.lastListing(alias)
	// At-or-over, not over: a peer whose observed cost equals the budget did not
	// answer inside it. That is exactly what a miss records -- the budget it was
	// still working at when the browse gave up.
	if !known || listing.TookMS < fleetHistoryPeerBudget.Milliseconds() {
		return fleetPeerListing{}, false
	}
	seen := listing.costSeenAt()
	if seen.IsZero() || !now.Before(seen.Add(fleetHistoryPeerRecheck)) {
		return fleetPeerListing{}, false
	}
	return listing, true
}

// collectConversations asks every target for its conversations and for the
// sessions it currently has running. Both come from the same daemon on
// purpose: a conversation that is live right now is not resumable — Sessions'
// own guard refuses it and tells you to attach instead — so a browser that
// could not tell the difference would print a command that fails on exactly
// the conversation the user is most likely to pick.
func (a *app) collectConversations(targets []fleetTarget, waitForPeers bool) (collectedConversations, error) {
	health := readFleetPeerHealth(a.home)
	now := a.now()
	outcomes := make([]historyTargetOutcome, len(targets))
	answers := make(chan historyTargetOutcome, len(targets))
	dispatched := make([]bool, len(targets))
	pending := 0
	awaitingLocal := false
	for index := range targets {
		target := targets[index]
		// The cooldown exists to keep the fast path fast, so it does not apply to
		// a caller who asked for the complete answer and accepted its cost.
		// Skipping a peer under --wait-for-peers would answer the flag with the
		// very shortfall line that recommends it.
		if target.Endpoint != localFleetEndpoint && !waitForPeers {
			if failure, retryAt, cooling := health.coolingDown(target.Alias, now); cooling {
				outcomes[index].err = fleetPeerSkipped(target, "history", failure, retryAt.Sub(now))
				continue
			}
			// A peer that has already shown it cannot answer inside the budget is
			// left out at no cost. Asking it again would spend the whole budget to
			// be told the same thing, which is exactly what a browse used to do.
			if listing, tooSlow := peerCannotAnswerInTime(health, target.Alias, now); tooSlow {
				outcomes[index].err = fleetHistoryPeerTooSlow(target, listing)
				continue
			}
		}
		outcomes[index] = historyTargetOutcome{index: index, err: errPending}
		dispatched[index] = true
		pending++
		awaitingLocal = awaitingLocal || target.Endpoint == localFleetEndpoint
		// A peer is normally held to the short fleet request timeout so one stale
		// machine cannot stall anything. --wait-for-peers is the caller saying
		// that is not the trade they want, and a peer capped at five seconds
		// while the local machine is allowed sixty would make the flag fail on
		// exactly the large history it exists to reach.
		timeout := fleetTargetTimeout(targets[index])
		if waitForPeers {
			timeout = localFleetRequestTimeout
		}
		go func(index int) {
			answers <- readTargetConversations(targets[index], index, timeout)
		}(index)
	}

	// The local machine owns the answer, so it is always awaited; peers only
	// add to it and are dropped once the budget passes. --wait-for-peers arms no
	// timer at all: the caller asked for the whole fleet, and every request is
	// already bounded by fleetTargetTimeout, so there is nothing left for a
	// second deadline to protect.
	peerBudget := fleetHistoryPeerBudget
	var budgetExpired <-chan time.Time
	expired := false
	if !waitForPeers {
		budget := time.NewTimer(peerBudget)
		defer budget.Stop()
		budgetExpired = budget.C
	}
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
		waited:   waitForPeers,
	}
	successes := 0
	rejection := ""
	for index, target := range targets {
		outcome := outcomes[index]
		state := historysearch.MachineState{
			Alias: target.Alias, Name: target.Name, Endpoint: target.Endpoint, Status: "listed",
		}
		// A peer that was still answering when the budget ran out is a healthy
		// peer with a large history, not a machine that is down, and the two must
		// not share a verdict. Cooling it down would skip it for five minutes on
		// the strength of it being busy, and — worse — would stop the very
		// browses that widen its budget from ever running, so the peer would be
		// dropped for being slow and then never given the chance to prove how
		// slow. What it gets instead is the lower bound it just demonstrated.
		stillAnswering := errors.Is(outcome.err, errPending)
		if stillAnswering {
			outcome.err = fleetHistoryTimedOut(target, peerBudget)
			if target.Endpoint != localFleetEndpoint {
				health.recordSlow(target.Alias, now, peerBudget)
			}
		}
		if outcome.err != nil {
			state.Status = "unavailable"
			state.Error = outcome.err.Error()
			collected.partial = true
			collected.machines = append(collected.machines, state)
			collected.withheld = append(collected.withheld,
				withheldFromLastListing(health, target, outcome.err))
			if refusal, refused := requestWasRejected(outcome.err); refused {
				if rejection == "" {
					rejection = refusal.Error()
				}
				health.recordSuccess(target.Alias)
			} else if dispatched[index] && !stillAnswering && target.Endpoint != localFleetEndpoint {
				health.recordFailure(target.Alias, now, outcome.err)
			}
			continue
		}
		successes++
		health.recordSuccess(target.Alias)
		if target.Endpoint != localFleetEndpoint {
			health.recordListing(target.Alias, now, len(outcome.listing.Sessions), outcome.took)
		}
		collected.machines = append(collected.machines, state)
		collected.known += len(outcome.listing.Sessions)
		for _, session := range outcome.listing.Sessions {
			collected.rows = append(collected.rows, conversationRow{
				target: index, Pinned: outcome.pinned[session.ID],
				LastHumanMessageAtMS: outcome.humanAt[session.ID],
			}.fill(target.Alias, qualify, session, outcome.live[session.ID]))
		}
	}
	health.save(a.home)
	if successes == 0 {
		return collectedConversations{}, fleetHistoryFailure(collected.machines, rejection)
	}
	return collected, nil
}

// errPending marks a target that has not answered yet. It is replaced with the
// real timeout message once the budget this browse actually granted is known,
// so the instruction a dropped peer prints names the wait the reader just paid
// rather than the constant it was floored at.
var errPending = errors.New("did not answer")

// withheldFromLastListing prices a machine that is missing from this answer,
// using the last browse that did reach it.
func withheldFromLastListing(
	health *fleetPeerHealth, target fleetTarget, reason error,
) withheldMachine {
	missing := withheldMachine{Alias: target.Alias, Name: target.Name, Reason: reason.Error()}
	listing, at, known := health.lastListing(target.Alias)
	if !known || !listing.Counted {
		return missing
	}
	missing.Counted = true
	missing.Conversations = listing.Conversations
	if !at.IsZero() {
		missing.CountedAt = at.Format(time.RFC3339)
		missing.countedAtMS = at.UnixMilli()
	}
	return missing
}

// readTargetConversations times its own round trip. What a peer costs is a
// measurement, not something to be assumed: the next browse reads it back to
// decide how long that peer is worth waiting for.
func readTargetConversations(
	target fleetTarget, index int, timeout time.Duration,
) (outcome historyTargetOutcome) {
	outcome = historyTargetOutcome{
		index: index, live: map[string]bool{}, pinned: map[string]bool{}, humanAt: map[string]int64{},
	}
	started := time.Now()
	defer func() { outcome.took = time.Since(started) }()
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
			outcome.pinned[value.ID] = value.Pinned
			if value.LastHumanMessageAt != nil {
				outcome.humanAt[value.ID] = *value.LastHumanMessageAt
			}
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
	r.MessagesUncounted = session.MessageCountUncounted
	r.PromptHistoryOnly = session.PromptHistoryOnly
	if session.Surface != nil {
		r.Surface = session.Surface.Display()
		r.SurfaceKind = session.Surface.Kind
		r.SurfaceRaw = session.Surface.Originator
		r.Actor = session.Surface.Actor
	}
	// When the conversation was last written to is the question a browser is
	// ordering by, and it is not the same as when the Sessions record was last
	// touched. A shutdown sweep that drains sixteen finished runners moves
	// every one of their record timestamps to the same instant; the transcripts
	// they name did not change, and it is the transcripts the user remembers.
	r.LastActiveAtMS = session.ConversationUpdatedAt
	r.ApproximateTime = session.ConversationUpdatedApproximate
	if r.LastActiveAtMS == 0 {
		r.LastActiveAtMS = session.LastActivityAt
	}
	if r.LastActiveAtMS > 0 {
		r.LastActiveAt = time.UnixMilli(r.LastActiveAtMS).Format(time.RFC3339)
	}
	// The daemon has always sent this and the row discarded it, so "when did
	// this start" could not be answered from the browse a person answers it in.
	r.StartedAtMS = session.CreatedAt
	if r.StartedAtMS > 0 {
		r.StartedAt = time.UnixMilli(r.StartedAtMS).Format(time.RFC3339)
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
// The second return value counts conversations dropped only because the daemon
// that listed them reported no provenance at all. Those rows are not evidence
// against the filter — they are evidence that the machine holding them is older
// than the field — and silently dropping them would turn "this machine needs
// updating" into "you have no Desktop conversations".
