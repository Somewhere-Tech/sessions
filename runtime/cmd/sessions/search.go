package main

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

func (a *app) cmdSearch(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return searchUsage()
	}
	if err := searchNeedsWords(args); err != nil {
		return err
	}
	queryText := args[0]
	args = args[1:]
	regex := removeFirst(&args, "--regex")
	exact := removeFirst(&args, "--exact")
	rankedFlag := removeFirst(&args, "--ranked")
	if (rankedFlag && regex) || (exact && regex) || (exact && rankedFlag) {
		return fail(1, "--exact, --regex, and --ranked cannot be combined")
	}
	ranked := !exact && !regex
	sessionID, hasSession := pluck(&args, "--session")
	role, hasRole := pluck(&args, "--role")
	tool, hasTool := pluck(&args, "--tool")
	name, hasName := pluck(&args, "--name")
	lane, hasLane := pluck(&args, "--lane")
	cwd, hasCWD := pluck(&args, "--cwd")
	since, hasSince := pluck(&args, "--since")
	until, hasUntil := pluck(&args, "--until")
	contextText, hasContext := pluck(&args, "--context")
	timeline := removeFirst(&args, "--timeline")
	limitText, hasLimit := pluck(&args, "-n")
	if len(args) != 0 {
		if strings.HasPrefix(queryText, "-") {
			return fail(1, "the query must come before flags, but %q looks like a flag used as the query.\ntry: sessions search %q %s ...\n%s", queryText, args[0], queryText, searchUsageText)
		}
		return fail(1, "unknown search option: %s\n%s", args[0], searchUsageText)
	}
	if hasSession && strings.TrimSpace(sessionID) == "" {
		return fail(1, "--session needs a session id")
	}
	role = strings.ToLower(role)
	if hasRole && role != "user" && role != "assistant" && role != "tool" {
		return fail(1, "--role must be \"user\", \"assistant\", or \"tool\"")
	}
	tool = strings.ToLower(tool)
	if hasTool && tool != "claude" && tool != "codex" && tool != "shell" {
		return fail(1, "--tool must be \"claude\", \"codex\", or \"shell\"")
	}
	limit := 0
	if hasLimit {
		parsed, err := strconv.Atoi(limitText)
		if err != nil || parsed < 1 || parsed > historysearch.MaxLimit {
			return fail(1, "-n must be between 1 and %d", historysearch.MaxLimit)
		}
		limit = parsed
	}
	contextCount := 0
	if hasContext {
		parsed, err := strconv.Atoi(contextText)
		if err != nil || parsed < 0 || parsed > historysearch.MaxContext {
			return fail(1, "--context must be between 0 and %d", historysearch.MaxContext)
		}
		contextCount = parsed
	}
	if hasName && hasLane {
		return fail(1, "--name and --lane are aliases; use only one")
	}

	parameters := url.Values{"q": {queryText}}
	if hasSession {
		parameters.Set("session", sessionID)
	}
	if hasRole {
		parameters.Set("role", role)
	}
	if hasTool {
		parameters.Set("tool", tool)
	}
	if hasName {
		parameters.Set("name", name)
	}
	if hasLane {
		parameters.Set("name", lane)
	}
	if hasCWD {
		parameters.Set("cwd", cwd)
	}
	if hasSince {
		parameters.Set("since", normalizeSearchDate(since, a.now(), false))
	}
	if hasUntil {
		parameters.Set("until", normalizeSearchDate(until, a.now(), true))
	}
	if hasContext {
		parameters.Set("context", strconv.Itoa(contextCount))
	}
	if timeline {
		parameters.Set("timeline", "true")
	}
	if regex {
		parameters.Set("regex", "true")
	}
	if ranked {
		parameters.Set("ranked", "true")
	}
	if hasLimit {
		parameters.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/search?" + parameters.Encode()
	var result historysearch.Response
	fleet := !a.explicitTarget
	if fleet {
		var err error
		result, err = a.searchApprovedFleet(path, limit, hasLimit, timeline, ranked)
		if err != nil {
			return err
		}
	} else if err := a.searchOneDaemon(path, &result); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if result.Partial {
		for _, machine := range result.Machines {
			if machine.Status == "unavailable" {
				fmt.Fprintf(a.stderr, "sessions: %s was unavailable: %s\n", machine.Name, machine.Error)
			}
		}
	}
	if err := a.writeSearchWidening(result); err != nil {
		return err
	}
	if len(result.Matches) == 0 {
		_, err := io.WriteString(a.stdout, "(no matches)\n")
		return err
	}
	for _, match := range result.Matches {
		name := match.Name
		if name == "" {
			name = "(unnamed)"
		}
		timestamp := "(no timestamp)"
		if match.Timestamp != nil && *match.Timestamp != "" {
			timestamp = *match.Timestamp
		}
		score := ""
		if ranked {
			score = "  " + rankedMatchLabel(match.Score)
		}
		displayRole := match.Role
		if match.Role == "tool" && match.Kind != "" {
			displayRole = match.Kind
		}
		identity := searchMatchIdentity(match.SessionID)
		if fleet && match.Reference != "" {
			identity = match.Reference
		}
		fmt.Fprintf(a.stdout, "%s  %s  %s%s\n  %s  %s  message %d\n",
			identity, name, match.Tool, score,
			displayRole, timestamp, match.MessageIndex+1)
		for _, message := range match.ContextBefore {
			fmt.Fprintf(a.stdout, "    before · %s: %s\n", message.Role, compactSearchText(message.Text))
		}
		fmt.Fprintf(a.stdout, "    %s\n", match.Snippet)
		for _, message := range match.ContextAfter {
			fmt.Fprintf(a.stdout, "    after · %s: %s\n", message.Role, compactSearchText(message.Text))
		}
		fmt.Fprintln(a.stdout)
	}
	return a.writeSearchRollup(result, queryText)
}

// writeSearchWidening explains a result set the reader did not ask for.
//
// Ranked search does not answer a phrase that matched nothing with nothing: it
// relaxes the conjunction and returns what the words could reach. That is the
// right answer and a confusing one to receive silently, because the rows that
// come back can share a single word with what was typed, and the reader's next
// move is to distrust search rather than to rephrase. `--json` has carried
// match_mode all along; this is the same fact for a person, and it stays quiet
// on a strict search of exactly what was typed.
//
// It used to say "No message had all of those words", which stopped being true.
// The ladder now leaves a rung that matched a handful as well as one that
// matched nothing, because on a sentence-length query a lone match is more
// likely a coincidence than an answer. Saying "no message" in that case tells
// the reader something false about their own corpus, and the one thing this
// sentence exists to do is keep them trusting what they are looking at.
func (a *app) writeSearchWidening(result historysearch.Response) error {
	relaxation := ""
	switch result.MatchMode {
	case "quorum":
		relaxation = "so it looked for the most distinctive of them"
	case "broad":
		relaxation = "so it looked for any one of them"
	default:
		return nil
	}
	if _, err := fmt.Fprintf(a.stdout,
		"Too few messages had all of those words, %s — expect looser matches.\n", relaxation); err != nil {
		return err
	}
	if result.EffectiveQuery != "" {
		if _, err := fmt.Fprintf(a.stdout, "  matched as: %s\n", result.EffectiveQuery); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(a.stdout)
	return err
}

// searchRollupSessions is how many conversations the footer names before it
// stops. Search's unit is the message and `sessions history` is the
// conversation-level browser, so the footer says which conversations these
// messages came from and hands the rest over rather than growing into a second
// browser.
const searchRollupSessions = 8

// writeSearchRollup answers "which conversations were those" under the page of
// messages. A reader given twenty rows across nine conversations was
// reconstructing that grouping by eye, and the count of hits beyond the page
// was visible only to a `--json` caller.
func (a *app) writeSearchRollup(result historysearch.Response, query string) error {
	if !searchRollupWorthPrinting(result) {
		return nil
	}
	parts := []string{
		searchRollupCount(result.TotalHits, "match", "matches", result.RollupPartial),
		"in " + searchRollupCount(result.TotalSessions, "conversation", "conversations", result.RollupPartial),
	}
	if shown := len(result.Matches); shown < result.TotalHits {
		parts = append(parts, fmt.Sprintf("showing %d", shown))
	}
	if _, err := fmt.Fprintln(a.stdout, strings.Join(parts, " · ")); err != nil {
		return err
	}
	shown := result.Sessions
	if len(shown) > searchRollupSessions {
		shown = shown[:searchRollupSessions]
	}
	identityWidth, nameWidth := 0, 0
	for _, rollup := range shown {
		identityWidth = max(identityWidth, len(searchMatchIdentity(rollup.SessionID)))
		nameWidth = max(nameWidth, len([]rune(searchRollupName(rollup))))
	}
	for _, rollup := range shown {
		line := fmt.Sprintf("  %-*s  %-*s  %s",
			identityWidth, searchMatchIdentity(rollup.SessionID),
			nameWidth, searchRollupName(rollup),
			searchRollupCount(rollup.Hits, "match", "matches", result.RollupPartial))
		if rollup.TitleMatch {
			line += " · title match"
		}
		if _, err := fmt.Fprintln(a.stdout, line); err != nil {
			return err
		}
	}
	if remaining := len(result.Sessions) - len(shown); remaining > 0 {
		_, err := fmt.Fprintf(a.stdout, "  … and %d more · browse them with %s\n",
			remaining, shellRecipe([]string{"sessions", "history", query}))
		return err
	}
	return nil
}

// searchRollupWorthPrinting keeps the footer off the screen when it would only
// repeat the rows above it. One conversation whose every hit is already on the
// page has been answered by the rows themselves; a daemon too old to send the
// rollup at all sends nothing here, and gets exactly the output it always had.
func searchRollupWorthPrinting(result historysearch.Response) bool {
	if len(result.Sessions) == 0 || result.TotalSessions == 0 {
		return false
	}
	return result.TotalSessions > 1 || result.RollupPartial ||
		result.TotalHits > len(result.Matches)
}

// searchRollupCount renders a count that must never read as complete when it
// is not. A truncated scan produces lower bounds, and a bare "12 matches"
// would state as fact something the scan never established.
func searchRollupCount(value int, singular, plural string, partial bool) string {
	unit := plural
	if value == 1 {
		unit = singular
	}
	if partial {
		return fmt.Sprintf("at least %d %s", value, unit)
	}
	return fmt.Sprintf("%d %s", value, unit)
}

func searchRollupName(rollup historysearch.SessionHits) string {
	if strings.TrimSpace(rollup.Name) == "" {
		return "(unnamed)"
	}
	return truncateRunes(rollup.Name, 40)
}

type fleetSearchOutcome struct {
	index    int
	response historysearch.Response
	err      error
}

// searchOneDaemon answers an explicitly targeted search with the same honesty
// the fleet path owes: a rejected query is the caller's to fix and keeps the
// daemon's own instruction, while an unreachable daemon is a transport failure.
func (a *app) searchOneDaemon(path string, result *historysearch.Response) error {
	err := getJSONFromClient(a.api, path, result, 0)
	if err == nil {
		return nil
	}
	if rejection, rejected := requestWasRejected(err); rejected {
		return fail(1, "%s", rejection.Error())
	}
	return fail(2, "search failed: %s", err)
}

func (a *app) searchApprovedFleet(
	path string,
	requestedLimit int,
	hasLimit bool,
	timeline bool,
	ranked bool,
) (historysearch.Response, error) {
	targets, err := a.approvedFleetTargets()
	if err != nil {
		return historysearch.Response{}, fail(2, "read approved machines: %s", err)
	}
	health := readFleetPeerHealth(a.home)
	now := a.now()
	outcomes := make([]fleetSearchOutcome, len(targets))
	answers := make(chan fleetSearchOutcome, len(targets))
	dispatched := make([]bool, len(targets))
	pending := 0
	awaitingLocal := false
	for index := range targets {
		target := targets[index]
		if target.Endpoint != localFleetEndpoint {
			if failure, retryAt, cooling := health.coolingDown(target.Alias, now); cooling {
				outcomes[index].err = fleetPeerSkipped(target, "search", failure, retryAt.Sub(now))
				if target.Owned {
					target.Client.close()
				}
				continue
			}
		}
		// A peer that never answers still has to leave a machine state behind,
		// so seed the outcome with the honest reason before dispatching it.
		outcomes[index] = fleetSearchOutcome{index: index, err: fleetPeerTimedOut(target)}
		dispatched[index] = true
		pending++
		awaitingLocal = awaitingLocal || target.Endpoint == localFleetEndpoint
		go func(index int) {
			outcome := fleetSearchOutcome{index: index}
			if targets[index].Owned {
				defer targets[index].Client.close()
			}
			outcome.err = getJSONFromClient(
				targets[index].Client, path, &outcome.response, fleetTargetTimeout(targets[index]),
			)
			answers <- outcome
		}(index)
	}

	// The local engine owns the answer, so it is always awaited. Peers only
	// enrich it, so they are dropped once the budget passes — and while the
	// local index is still building, they answer into a wait already being paid.
	budget := time.NewTimer(fleetPeerBudget)
	defer budget.Stop()
	budgetExpired := budget.C
	expired := false
	accept := func(outcome fleetSearchOutcome) {
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

	result := historysearch.Response{
		Matches:  make([]historysearch.Match, 0),
		Machines: make([]historysearch.MachineState, 0, len(targets)),
	}
	successes := 0
	rejection := ""
	seen := make(map[string]int)
	rolledUp := make(map[string]int)
	for index, target := range targets {
		outcome := outcomes[index]
		state := historysearch.MachineState{
			Alias: target.Alias, Name: target.Name, Endpoint: target.Endpoint, Status: "searched",
		}
		if outcome.err != nil {
			state.Status = "unavailable"
			state.Error = outcome.err.Error()
			result.Partial = true
			result.Machines = append(result.Machines, state)
			if refusal, refused := requestWasRejected(outcome.err); refused {
				// The machine is healthy and said why; that is not a peer
				// failure and must not be cached as one.
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
		result.Machines = append(result.Machines, state)
		// The session rollup answers the question an agent actually asks --
		// which session was that -- so it has to survive the fleet merge like
		// matches do. Without this it was computed per machine and thrown
		// away, and only a single-daemon caller ever saw it.
		result.TotalHits += outcome.response.TotalHits
		if outcome.response.RollupPartial {
			result.RollupPartial = true
		}
		if result.EffectiveQuery == "" {
			result.EffectiveQuery = outcome.response.EffectiveQuery
			result.MatchMode = outcome.response.MatchMode
		} else if outcome.response.EffectiveQuery != "" &&
			outcome.response.EffectiveQuery != result.EffectiveQuery {
			// Two machines interpreted the same query differently, which means
			// they are running different versions. Saying which one produced
			// these results would be a guess, so say neither.
			result.EffectiveQuery = ""
			result.MatchMode = ""
			result.RollupPartial = true
		}
		for _, rollup := range outcome.response.Sessions {
			rollup.Machine = target.Alias
			key := qualifiedHistoryReference(target.Alias, rollup.SessionID)
			rollup.SessionID = key
			if previous, duplicate := rolledUp[key]; duplicate {
				result.Sessions[previous] = mergeSessionHits(result.Sessions[previous], rollup)
				continue
			}
			rolledUp[key] = len(result.Sessions)
			result.Sessions = append(result.Sessions, rollup)
		}
		for _, match := range outcome.response.Matches {
			match.MachineAlias = target.Alias
			match.Reference = qualifiedHistoryReference(target.Alias, match.SessionID)
			match.AvailableOn = []string{target.Alias}
			key := fleetSearchMatchKey(match)
			if previous, duplicate := seen[key]; duplicate {
				result.Matches[previous].AvailableOn = append(
					result.Matches[previous].AvailableOn, target.Alias,
				)
				continue
			}
			seen[key] = len(result.Matches)
			result.Matches = append(result.Matches, match)
		}
	}
	health.save(a.home)
	if successes == 0 {
		return historysearch.Response{}, fleetSearchFailure(result.Machines, rejection)
	}
	sortFleetSearchMatches(result.Matches, timeline, ranked)
	limit := historysearch.DefaultLimit
	if hasLimit {
		limit = requestedLimit
	}
	if len(result.Matches) > limit {
		result.Matches = result.Matches[:limit]
	}
	result.Total = len(result.Matches)
	result.TotalSessions = len(result.Sessions)
	sortFleetSessionHits(result.Sessions)
	return result, nil
}

// mergeSessionHits folds one session observed on two machines into a single
// row. The same conversation reachable from two places is one answer to
// "which session", not two.
func mergeSessionHits(into, from historysearch.SessionHits) historysearch.SessionHits {
	into.Hits += from.Hits
	into.TitleMatch = into.TitleMatch || from.TitleMatch
	if from.Score > into.Score {
		into.Score = from.Score
	}
	if into.FirstHitAt == "" || (from.FirstHitAt != "" && from.FirstHitAt < into.FirstHitAt) {
		into.FirstHitAt = from.FirstHitAt
	}
	if from.LastHitAt > into.LastHitAt {
		into.LastHitAt = from.LastHitAt
	}
	if len(into.Snippets) == 0 {
		into.Snippets = from.Snippets
	}
	return into
}

// sortFleetSessionHits puts the strongest answer first. A session whose own
// name satisfied the query outranks one that merely mentioned the words,
// because someone who remembers a title remembers it exactly.
func sortFleetSessionHits(sessions []historysearch.SessionHits) {
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].TitleMatch != sessions[j].TitleMatch {
			return sessions[i].TitleMatch
		}
		if sessions[i].Score != sessions[j].Score {
			return sessions[i].Score > sessions[j].Score
		}
		return sessions[i].Hits > sessions[j].Hits
	})
}

// fleetSearchFailure explains why nothing answered. A rejected query is never
// reported as a network problem — that sends the reader to debug a LAN that is
// working — and a transport failure names every machine that was tried,
// whatever the size of the fleet.
func fleetSearchFailure(machines []historysearch.MachineState, rejection string) error {
	if rejection != "" {
		return fail(1, "%s", rejection)
	}
	lines := make([]string, 0, len(machines))
	for _, machine := range machines {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", machine.Alias, machine.Name, machine.Error))
	}
	if len(lines) == 0 {
		return fail(2, "no approved Sessions machine answered this search")
	}
	return fail(2, "no approved Sessions machine answered this search:\n%s", strings.Join(lines, "\n"))
}

func fleetPeerTimedOut(target fleetTarget) error {
	if target.Endpoint == localFleetEndpoint {
		return fmt.Errorf("this machine did not answer within %s", localFleetRequestTimeout)
	}
	return fmt.Errorf(
		"did not answer within %s, so its matches are missing; run `sessions --machine %s search ...` to wait for it",
		fleetPeerBudget, target.Alias,
	)
}

// fleetPeerSkipped names the command the reader was actually running. The
// advice only helps if it retries their own command; a history browse that
// told them to re-run a search would send them somewhere else.
func fleetPeerSkipped(target fleetTarget, command string, failure fleetPeerFailure, retryIn time.Duration) error {
	return fmt.Errorf(
		"skipped after a recent failure (%s), retried automatically in %s; run `sessions --machine %s %s ...` to try it now",
		failure.Error, retryIn.Round(time.Second), target.Alias, command,
	)
}

func sortFleetSearchMatches(matches []historysearch.Match, timeline, ranked bool) {
	sort.SliceStable(matches, func(leftIndex, rightIndex int) bool {
		left := matches[leftIndex]
		right := matches[rightIndex]
		if !timeline && ranked && left.Score != right.Score {
			return left.Score > right.Score
		}
		leftTime, leftOK := fleetSearchTimestamp(left.Timestamp)
		rightTime, rightOK := fleetSearchTimestamp(right.Timestamp)
		if leftOK != rightOK {
			return leftOK
		}
		if !leftTime.Equal(rightTime) {
			if timeline {
				return leftTime.Before(rightTime)
			}
			return leftTime.After(rightTime)
		}
		if left.Reference != right.Reference {
			return left.Reference < right.Reference
		}
		return left.MessageIndex < right.MessageIndex
	})
}

func fleetSearchTimestamp(value *string) (time.Time, bool) {
	if value == nil || *value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	return parsed, err == nil
}

func fleetSearchMatchKey(match historysearch.Match) string {
	if match.ProviderSessionID != "" && match.MessageID != "" {
		return match.Tool + "\x00" + match.ProviderSessionID + "\x00" + match.MessageID
	}
	return match.MachineAlias + "\x00" + match.SessionID + "\x00" + match.MessageID
}

func (a *app) cmdGrep(args []string) error {
	normalized, err := normalizeGrepArgs(args)
	if err != nil {
		return err
	}
	return a.cmdSearch(normalized)
}

func normalizeGrepArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fail(1, "usage: sessions grep [options] <query>")
	}
	query := ""
	options := make([]string, 0, len(args))
	valueOptions := map[string]bool{
		"--session": true, "--role": true, "--tool": true, "--name": true,
		"--lane": true, "--cwd": true, "--since": true, "--until": true,
		"--context": true, "-C": true, "-n": true,
	}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "-i" {
			continue
		}
		if strings.HasPrefix(argument, "-C") && len(argument) > 2 {
			options = append(options, "--context", argument[2:])
			continue
		}
		if valueOptions[argument] {
			if index+1 >= len(args) {
				return nil, fail(1, "%s needs a value", argument)
			}
			name := argument
			if name == "-C" {
				name = "--context"
			}
			options = append(options, name, args[index+1])
			index++
			continue
		}
		if strings.HasPrefix(argument, "-") {
			options = append(options, argument)
			continue
		}
		if query != "" {
			return nil, fail(1, "grep accepts one query; quote phrases with spaces")
		}
		query = argument
	}
	if query == "" {
		return nil, fail(1, "sessions grep needs a query")
	}
	return append([]string{query}, options...), nil
}

// searchMatchIdentity prints an id the reader can hand straight back to
// `sessions cat` or `sessions resume`. Provider history ids are namespaced
// (`provider:codex:019f...`), so the eight-character form used for opaque
// session ids collapses every one of them to the word "provider" and resolves
// to nothing.
func searchMatchIdentity(sessionID string) string {
	if strings.Contains(sessionID, ":") {
		return sessionID
	}
	return prefixString(sessionID, 8)
}

func rankedMatchLabel(score float64) string {
	switch {
	case score >= 0.85:
		return "best match"
	case score >= 0.5:
		return "strong match"
	default:
		return "related"
	}
}

func compactSearchText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		return value[:239] + "…"
	}
	return value
}

// searchValueOptions are the search flags that consume the argument after
// them, so a value is never mistaken for the query.
var searchValueOptions = map[string]bool{
	"--session": true, "--role": true, "--tool": true, "--name": true,
	"--lane": true, "--cwd": true, "--since": true, "--until": true,
	"--context": true, "-n": true,
}

// searchNarrowingOptions are the filters that also narrow a conversation
// browse, so a filter-only search can be handed to `sessions history` intact.
var searchNarrowingOptions = map[string]bool{
	"--session": true, "--tool": true, "--name": true,
	"--lane": true, "--cwd": true, "--since": true, "--until": true,
}

// searchNeedsWords answers the request that used to be refused as a typo. A
// caller who typed only filters — `sessions search --since today --tool codex`
// — is asking which conversations they had, not which messages contain a word,
// and the old refusal ("the query must come before flags") sent them looking
// for a word they would have had to remember in order to ask at all. Search's
// unit is the message and it genuinely needs words; the browse they wanted is
// a different command, so hand them that exact command rather than a rule.
func searchNeedsWords(args []string) error {
	positionals, narrowing := searchArgumentShape(args)
	if len(positionals) > 0 {
		return nil
	}
	if len(narrowing) == 0 {
		return searchUsage()
	}
	return fail(1,
		"sessions search looks for words inside conversations, so it needs some.\n"+
			"Filters alone describe conversations, not words — browse them with:\n  %s\n\n%s",
		shellRecipe(append([]string{"sessions", "history"}, narrowing...)), searchUsageText)
}

// searchArgumentShape reports which arguments were meant as the query and
// which narrowing filters were supplied, without interpreting either.
func searchArgumentShape(args []string) (positionals, narrowing []string) {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		if !searchValueOptions[argument] {
			continue
		}
		value := ""
		if index+1 < len(args) {
			value = args[index+1]
			index++
		}
		if searchNarrowingOptions[argument] {
			narrowing = append(narrowing, argument, value)
		}
	}
	return positionals, narrowing
}

// normalizeSearchDate lets search accept the same spoken dates the browser
// does. The daemon's search route understands only YYYY-MM-DD and RFC3339, so
// anything it already understands is passed through untouched and keeps the
// daemon's own error message; only the spoken forms are resolved here.
func normalizeSearchDate(raw string, now time.Time, endOfDay bool) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value != "today" && value != "yesterday" {
		if _, ok := parseHistoryDuration(value); !ok {
			return raw
		}
	}
	millis, _, err := parseHistoryTime(raw, now, endOfDay)
	if err != nil {
		return raw
	}
	return time.UnixMilli(millis).Format(time.RFC3339)
}

const searchUsageText = "usage: sessions search <query> [--session ID[,ID...]] [--role user|assistant|tool] [--tool claude|codex|shell] [--name GLOB] [--cwd PATH] [--since DATE] [--until DATE] [--context N] [--timeline] [-n N] [--exact | --regex | --ranked] [--json]\nranked token search is the default; use --exact for a contiguous literal phrase. The query comes FIRST, before any flags"

func searchUsage() error {
	return fail(1, searchUsageText)
}
