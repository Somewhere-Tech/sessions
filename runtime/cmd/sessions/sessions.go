package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

type sessionRecord struct {
	value session
	raw   json.RawMessage
}

type ownershipScope struct {
	kind           string
	id             string
	osUserFallback bool
}

type sessionsListOptions struct {
	mine          bool
	all           bool
	owner         string
	includeClosed bool
}

type lsListOptions struct {
	mine          bool
	all           bool
	includeClosed bool
	asideOnly     bool
	notAside      bool
}

// Selecting which records come back has two independent axes, and conflating
// them is the single most common way an agent mis-reads these commands. State
// is widened by -a; owner is widened by --all-owners. Each axis has exactly one
// canonical spelling accepted identically by ls, list, and lanes, with the
// historical spellings retained as aliases so existing scripts keep working.
const (
	// includeEndedCanonicalFlag is the long canonical spelling; -a is its short
	// form and --include-closed is the older `list` spelling kept as an alias.
	includeEndedCanonicalFlag = "--include-exited"
	// allOwnersCanonicalFlag is the unambiguous spelling of the owner axis.
	// --all is the historical alias and means the same thing: every owner, not
	// every state.
	allOwnersCanonicalFlag = "--all-owners"

	lsUsageText    = "usage: sessions ls [--mine | --all-owners] [-a | --include-exited] [--aside | --not-aside] [--kind lane]"
	listUsageText  = "usage: sessions list [--mine | --owner ID | --all-owners] [-a | --include-exited]"
	lanesUsageText = "usage: sessions lanes [--all-owners | --mine [--owner ID] | --subtree ID] [--direct] [--detach]"

	// selectionAxisHint is appended to every list-surface usage error because
	// the failure it prevents is silent: an agent that reaches for --all when
	// it wanted ended records gets a plausible-looking answer to the wrong
	// question.
	selectionAxisHint = "state: -a (long form --include-exited, alias --include-closed) also returns ended sessions and lanes\n" +
		"owner: --all-owners (alias --all) returns every owner's records — it does not change which states are shown"
)

// isIncludeEndedFlag reports whether an argument selects the ended-records
// axis. All three spellings mean exactly one thing on every command that
// accepts them.
func isIncludeEndedFlag(argument string) bool {
	switch argument {
	case "-a", includeEndedCanonicalFlag, "--include-closed":
		return true
	}
	return false
}

// isAllOwnersFlag reports whether an argument selects the all-owners axis.
func isAllOwnersFlag(argument string) bool {
	return argument == allOwnersCanonicalFlag || argument == "--all"
}

func unknownListOption(command, argument, usageText string) error {
	return fail(1, "unknown %s option %s\n%s\n%s", command, argument, usageText, selectionAxisHint)
}

func parseLSListOptions(args []string) (lsListOptions, error) {
	options := lsListOptions{}
	for _, arg := range args {
		switch {
		case arg == "--mine":
			options.mine = true
		case isAllOwnersFlag(arg):
			options.all = true
		case isIncludeEndedFlag(arg):
			options.includeClosed = true
		case arg == "--aside":
			options.asideOnly = true
		case arg == "--not-aside":
			options.notAside = true
		default:
			return options, unknownListOption("ls", arg, lsUsageText)
		}
	}
	if options.mine && options.all {
		return options, fail(1, "--mine and --all-owners are mutually exclusive")
	}
	if options.asideOnly && options.notAside {
		return options, fail(1, "--aside and --not-aside are mutually exclusive")
	}
	return options, nil
}

func parseSessionsListOptions(args []string) (sessionsListOptions, error) {
	options := sessionsListOptions{}
	for index := 0; index < len(args); index++ {
		switch argument := args[index]; {
		case argument == "--mine":
			options.mine = true
		case isAllOwnersFlag(argument):
			options.all = true
		case isIncludeEndedFlag(argument):
			options.includeClosed = true
		case argument == "--owner":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "--") {
				return options, fail(1, "--owner needs a non-empty id")
			}
			options.owner = strings.TrimSpace(args[index+1])
			index++
		default:
			return options, unknownListOption("list", argument, listUsageText)
		}
	}
	selectors := 0
	if options.mine {
		selectors++
	}
	if options.owner != "" {
		selectors++
	}
	if options.all {
		selectors++
	}
	if selectors > 1 {
		return options, fail(1, "--mine, --owner, and --all-owners are mutually exclusive")
	}
	return options, nil
}

func (a *app) cmdSessions(args []string) error {
	options, err := parseSessionsListOptions(args)
	if err != nil {
		return err
	}
	records, err := a.fetchSessionRecords(options.includeClosed)
	if err != nil {
		return err
	}
	var scope ownershipScope
	if options.mine || options.owner != "" {
		scope, err = a.resolveOwnershipScope(options.owner, "")
		if err != nil {
			return err
		}
		records = filterSessionRecords(records, func(value session) bool {
			return matchesOwnership(value, scope, false)
		})
	}
	// Pinned first in both formats. The JSON caller is the one that most needs
	// it: an agent reading a long listing takes the head of the array.
	records = pinnedFirst(records)
	if a.wantJSON {
		return writeRawSessionRecords(a.stdout, records)
	}
	if scope.osUserFallback {
		writeOSUserScope(a.stdout, scope)
	}
	if len(records) == 0 {
		_, err := io.WriteString(a.stdout, "(no sessions or lanes)\n")
		return err
	}
	showProfile := recordsHaveProfiles(records)
	showPin := recordsHavePins(records)
	header := []string{"ID", "TYPE", "NAME", "DESC", "TOOL"}
	if showProfile {
		header = append(header, "PROFILE")
	}
	if showPin {
		header = append(header, "PIN")
	}
	header = append(header, "CWD", "STATE", "SUMMARY", "AGE", "OWNER")
	rows := [][]string{header}
	for _, record := range records {
		value := record.value
		row := []string{
			prefixString(value.ID, 8), sessionType(value), compactSessionName(value.Name), compactDescription(value.Description), toolOfSession(value),
		}
		if showProfile {
			row = append(row, compactProfile(value.Profile))
		}
		if showPin {
			row = append(row, pinMark(value))
		}
		row = append(
			row,
			strings.Replace(value.Cwd, a.home, "~", 1),
			sessionState(value),
			compactSummary(value.LastSummary),
			a.ageOf(value.CreatedAt),
			ownershipLabel(value),
		)
		rows = append(rows, row)
	}
	return writePaddedRows(a.stdout, rows)
}

func (a *app) fetchSessionRecords(includeClosed bool) ([]sessionRecord, error) {
	path := "/api/sessions"
	if includeClosed {
		path += "?include_exited=1"
	}
	response, err := a.api.request(context.Background(), "GET", path, nil, 0)
	if err != nil {
		return nil, err
	}
	if response.status >= 400 {
		return nil, fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	var envelope struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil {
		return nil, err
	}
	records := make([]sessionRecord, 0, len(envelope.Sessions))
	for _, raw := range envelope.Sessions {
		var value session
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		records = append(records, sessionRecord{value: value, raw: append(json.RawMessage(nil), raw...)})
	}
	return records, nil
}

func (a *app) resolveOwnershipScope(explicitOwner, explicitSession string) (ownershipScope, error) {
	if explicitOwner != "" {
		return ownershipScope{kind: "external", id: explicitOwner}, nil
	}
	if explicitSession != "" {
		return ownershipScope{kind: "session", id: explicitSession}, nil
	}
	ownerEnvironment := os.Getenv("SESSIONS_OWNER_ID")
	if ownerEnvironment != "" && strings.TrimSpace(ownerEnvironment) != ownerEnvironment {
		return ownershipScope{}, fail(1, "SESSIONS_OWNER_ID must not contain surrounding whitespace")
	}
	if ownerEnvironment != "" {
		return ownershipScope{kind: "external", id: ownerEnvironment}, nil
	}
	sessionEnvironment := os.Getenv("SESSIONS_SESSION_ID")
	if sessionEnvironment != "" && strings.TrimSpace(sessionEnvironment) != sessionEnvironment {
		return ownershipScope{}, fail(1, "SESSIONS_SESSION_ID must not contain surrounding whitespace")
	}
	if sessionEnvironment != "" {
		if !looksLikeLaneID(sessionEnvironment) {
			return ownershipScope{}, fail(1, "SESSIONS_SESSION_ID is not a session UUID")
		}
		return ownershipScope{kind: "session", id: sessionEnvironment}, nil
	}
	userID, err := a.daemonUserCreatorID()
	if err != nil {
		return ownershipScope{}, err
	}
	return ownershipScope{kind: "user", id: userID, osUserFallback: true}, nil
}

func (a *app) daemonUserCreatorID() (string, error) {
	var response lanesResponse
	if err := a.getJSON("/api/lanes", &response); err != nil {
		return "", err
	}
	if response.UserCreatorID != "" {
		return response.UserCreatorID, nil
	}
	// Compatibility with daemons that predate the principal hint.
	return ledger.LocalUserCreatorID()
}

func matchesOwnership(value session, scope ownershipScope, direct bool) bool {
	if scope.kind == "session" {
		if direct {
			return value.CreatorKind == "session" && value.CreatorID == scope.id
		}
		for _, ancestor := range value.CreatorAncestry {
			if ancestor == scope.id {
				return true
			}
		}
		return len(value.CreatorAncestry) == 0 && value.CreatorKind == "session" && value.CreatorID == scope.id
	}
	rootKind, rootID := value.RootCreatorKind, value.RootCreatorID
	if rootKind == "" && value.CreatorKind != "session" {
		rootKind, rootID = value.CreatorKind, value.CreatorID
	}
	return rootKind == scope.kind && rootID == scope.id
}

// pinnedFirst floats the sessions the user marked as workbenches to the top of
// a listing without disturbing anything else about the order.
//
// This is the reason the mark exists. A list that is a hundred and eighty
// records deep is not searchable by eye, and the handful of sessions a person
// actually works in are scattered through it by whatever order the daemon
// happened to return. The sort is stable so the existing order — which every
// other flag and both list surfaces already agree on — is exactly preserved
// within each of the two groups.
func pinnedFirst(records []sessionRecord) []sessionRecord {
	sorted := append([]sessionRecord(nil), records...)
	sort.SliceStable(sorted, func(left, right int) bool {
		return sorted[left].value.Pinned && !sorted[right].value.Pinned
	})
	return sorted
}

// recordsHavePins reports whether the PIN column would say anything. It follows
// the PROFILE column's rule: a column that is a dash on every row is noise on
// every row.
func recordsHavePins(records []sessionRecord) bool {
	for _, record := range records {
		if record.value.Pinned {
			return true
		}
	}
	return false
}

func pinMark(value session) string {
	if value.Pinned {
		return "pin"
	}
	return "-"
}

func filterSessionRecords(records []sessionRecord, keep func(session) bool) []sessionRecord {
	filtered := make([]sessionRecord, 0, len(records))
	for _, record := range records {
		if keep(record.value) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func writeRawSessionRecords(writer io.Writer, records []sessionRecord) error {
	raw := make([]json.RawMessage, 0, len(records))
	for _, record := range records {
		raw = append(raw, record.raw)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, encoded, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')
	_, err = formatted.WriteTo(writer)
	return err
}

func writeOSUserScope(writer io.Writer, scope ownershipScope) {
	_, _ = fmt.Fprintf(writer, "ownership scope: OS user %s (no SESSIONS_OWNER_ID or SESSIONS_SESSION_ID)\n", scope.id)
}

func sessionType(value session) string {
	if value.Kind == "lane" {
		return "lane"
	}
	return "session"
}

func compactSessionName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "-"
	}
	return strings.Join(strings.Fields(name), " ")
}

func recordsHaveProfiles(records []sessionRecord) bool {
	for _, record := range records {
		if record.value.Profile != "" {
			return true
		}
	}
	return false
}

func compactProfile(profile string) string {
	if profile == "" {
		return "-"
	}
	return profile
}

func compactDescription(description string) string {
	description = strings.Join(strings.Fields(description), " ")
	if description == "" {
		return "-"
	}
	const maximum = 40
	runes := []rune(description)
	if len(runes) > maximum {
		return string(runes[:maximum-1]) + "…"
	}
	return description
}

func compactSummary(summary string) string {
	summary = strings.Join(strings.Fields(summary), " ")
	if summary == "" {
		return "-"
	}
	const maximum = 64
	runes := []rune(summary)
	if len(runes) > maximum {
		return string(runes[:maximum-1]) + "…"
	}
	return summary
}

func sessionState(value session) string {
	if value.Exited {
		code := "∅"
		if value.ExitCode != nil {
			code = strconv.Itoa(*value.ExitCode)
		}
		if value.ExitSignal != nil && *value.ExitSignal != "" {
			code += " " + *value.ExitSignal
		}
		return "exited(" + code + ")"
	}
	if value.SetAsideAt != nil {
		return "set-aside"
	}
	if value.Kind == "lane" {
		return "running"
	}
	if value.Working {
		return "working"
	}
	switch value.IdleReason {
	case "needs-input":
		return "needs-you"
	case "failed":
		return "failed"
	case "completed":
		return "finished"
	case "never-started":
		return "not-started"
	}
	return "idle"
}

func ownershipLabel(value session) string {
	kind, id := value.RootCreatorKind, value.RootCreatorID
	if kind == "" {
		kind, id = value.CreatorKind, value.CreatorID
	}
	if kind == "" || id == "" {
		return "-"
	}
	if kind == "session" {
		id = prefixString(id, 8)
	}
	return kind + ":" + id
}

func writePaddedRows(writer io.Writer, rows [][]string) error {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for column, cell := range row {
			if jsLength(cell) > widths[column] {
				widths[column] = jsLength(cell)
			}
		}
	}
	for _, row := range rows {
		for column, cell := range row {
			if column > 0 {
				if _, err := io.WriteString(writer, "  "); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(writer, cell); err != nil {
				return err
			}
			if _, err := io.WriteString(writer, strings.Repeat(" ", widths[column]-jsLength(cell))); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(writer, "\n"); err != nil {
			return err
		}
	}
	return nil
}
