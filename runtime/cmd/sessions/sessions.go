package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

func parseLSListOptions(args []string) (lsListOptions, error) {
	options := lsListOptions{}
	for _, arg := range args {
		switch arg {
		case "--mine":
			options.mine = true
		case "--all":
			options.all = true
		case "-a", "--include-exited", "--include-closed":
			options.includeClosed = true
		case "--aside":
			options.asideOnly = true
		case "--not-aside":
			options.notAside = true
		default:
			return options, fail(1, "usage: sessions ls [--mine | --all] [-a | --include-exited] [--aside | --not-aside]")
		}
	}
	if options.mine && options.all {
		return options, fail(1, "--mine and --all are mutually exclusive")
	}
	if options.asideOnly && options.notAside {
		return options, fail(1, "--aside and --not-aside are mutually exclusive")
	}
	return options, nil
}

func parseSessionsListOptions(args []string) (sessionsListOptions, error) {
	options := sessionsListOptions{}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--mine":
			options.mine = true
		case "--all":
			options.all = true
		case "--include-closed":
			options.includeClosed = true
		case "--owner":
			if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" || strings.HasPrefix(args[index+1], "--") {
				return options, fail(1, "--owner needs a non-empty id")
			}
			options.owner = strings.TrimSpace(args[index+1])
			index++
		default:
			return options, fail(1, "usage: sessions list [--mine | --owner ID | --all] [--include-closed]")
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
		return options, fail(1, "--mine, --owner, and --all are mutually exclusive")
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
	header := []string{"ID", "TYPE", "NAME", "DESC", "TOOL"}
	if showProfile {
		header = append(header, "PROFILE")
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
