package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/somewhere-tech/sessions/runtime/internal/ansi"
)

type session struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name,omitempty"`
	Description        string            `json:"description"`
	DescriptionSource  string            `json:"description_source,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Kind               string            `json:"kind,omitempty"`
	Cmd                string            `json:"cmd"`
	Args               []string          `json:"args"`
	Cwd                string            `json:"cwd"`
	Profile            string            `json:"profile,omitempty"`
	ConfigDir          string            `json:"config_dir,omitempty"`
	WorktreePath       string            `json:"worktree_path,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	Base               string            `json:"base,omitempty"`
	SourceRepo         string            `json:"source_repo,omitempty"`
	Cols               int               `json:"cols"`
	Rows               int               `json:"rows"`
	CreatedAt          int64             `json:"createdAt"`
	PID                int               `json:"pid"`
	RunnerProtocol     int               `json:"runnerProtocol"`
	RunnerVersion      string            `json:"runnerVersion,omitempty"`
	Tool               string            `json:"tool"`
	Working            bool              `json:"working"`
	LastDataAt         int64             `json:"lastDataAt"`
	LastUserMessageAt  *int64            `json:"lastUserMessageAt"`
	IdleReason         string            `json:"idleReason,omitempty"`
	IdleDetail         string            `json:"idleDetail,omitempty"`
	IdleSince          *int64            `json:"idleSince,omitempty"`
	LastSummary        string            `json:"lastSummary,omitempty"`
	Model              string            `json:"model,omitempty"`
	Effort             string            `json:"effort,omitempty"`
	Exited             bool              `json:"exited"`
	ExitCode           *int              `json:"exitCode"`
	ExitSignal         *string           `json:"exitSignal"`
	ExitedAt           *int64            `json:"exitedAt"`
	ConversationID     string            `json:"conversationId,omitempty"`
	RemoteEndpoint     string            `json:"remoteEndpoint,omitempty"`
	ClaudeSessionID    string            `json:"claudeSessionId,omitempty"`
	CreatorKind        string            `json:"creator_kind,omitempty"`
	CreatorID          string            `json:"creator_id,omitempty"`
	ParentSessionID    string            `json:"parent_session_id,omitempty"`
	DelegationKind     string            `json:"delegation_kind,omitempty"`
	Permissions        string            `json:"permissions,omitempty"`
	Lifecycle          string            `json:"lifecycle,omitempty"`
	SetAsideAt         *int64            `json:"setAsideAt,omitempty"`
	Pinned             bool              `json:"pinned"`
	CreatorAncestry    []string          `json:"creator_ancestry,omitempty"`
	RootCreatorKind    string            `json:"root_creator_kind,omitempty"`
	RootCreatorID      string            `json:"root_creator_id,omitempty"`
	ProvenanceStatus   string            `json:"provenance_status,omitempty"`
	ReopenedAs         string            `json:"reopened_as,omitempty"`
	ResumedFrom        string            `json:"resumed_from,omitempty"`
	MovedToEndpoint    string            `json:"moved_to_endpoint,omitempty"`
	MovedToSessionID   string            `json:"moved_to_session_id,omitempty"`
	MovedFromEndpoint  string            `json:"moved_from_endpoint,omitempty"`
	MovedFromSessionID string            `json:"moved_from_session_id,omitempty"`
	EndedByKind        string            `json:"ended_by_kind,omitempty"`
	EndedByID          string            `json:"ended_by_id,omitempty"`
	EndedByName        string            `json:"ended_by_name,omitempty"`
	EndedByClient      string            `json:"ended_by_client,omitempty"`
	EndReason          string            `json:"end_reason,omitempty"`
	EndOperationID     string            `json:"end_operation_id,omitempty"`
	Extra              json.RawMessage   `json:"-"`
}

type sessionsResponse struct {
	Sessions []session `json:"sessions"`
}

func (a *app) listSessions(includeExited bool) ([]session, error) {
	path := "/api/sessions"
	if includeExited {
		path += "?include_exited=1"
	}
	var response sessionsResponse
	if err := a.getJSON(path, &response); err != nil {
		return nil, err
	}
	return response.Sessions, nil
}

func classifyTool(command string) string {
	command = strings.ToLower(command)
	if strings.HasSuffix(command, "/claude") || command == "claude" {
		return "claude"
	}
	if strings.HasSuffix(command, "/codex") || command == "codex" {
		return "codex"
	}
	if command == "" {
		return "shell"
	}
	return filepath.Base(command)
}

func toolOfSession(value session) string {
	if value.Tool != "" {
		return value.Tool
	}
	return classifyTool(value.Cmd)
}

func shortToolName(tool string) string {
	if tool == "claude-code" {
		return "claude"
	}
	if tool == "" {
		return "shell"
	}
	return tool
}

func isConfirmableTool(tool string) bool { return tool == "claude-code" || tool == "codex" }

func unknownSessionMessage(id string) string {
	// No invented causes and no errands. The old text blamed "a stale id
	// after a daemon restart" -- a guess, usually wrong -- and told the caller
	// to go run `sessions ls`, which is the servant handing the master a
	// chore. Per docs/PHILOSOPHY.md, an id that ever existed must resolve
	// forever; until wake-on-message makes that true for writes, this message
	// at least tells the truth and points at the durable record.
	return fmt.Sprintf("no live session matches '%s'; if it ever existed its conversation is preserved — `sessions history %s` finds it and `sessions resume` reopens it", id, id)
}

func (a *app) sessionLabel(value session) string {
	parts := []string{shortToolName(toolOfSession(value))}
	if value.Name != "" {
		parts = append(parts, value.Name)
	}
	if value.Cwd != "" {
		parts = append(parts, strings.Replace(value.Cwd, a.home, "~", 1))
	}
	if value.Exited {
		parts = append(parts, "exited")
	} else if value.Working {
		parts = append(parts, "working")
	} else {
		parts = append(parts, "idle")
	}
	return strings.Join(parts, " ")
}

func (a *app) resolveSessionID(idOrPrefix string) (string, error) {
	sessions, err := a.listSessions(true)
	if err != nil {
		return "", err
	}
	for _, candidate := range sessions {
		if candidate.ID == idOrPrefix {
			return candidate.ID, nil
		}
	}
	matches := make([]session, 0)
	for _, candidate := range sessions {
		if strings.HasPrefix(candidate.ID, idOrPrefix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) == 0 {
		return "", fail(1, "%s", unknownSessionMessage(idOrPrefix))
	}
	var lines strings.Builder
	for _, candidate := range matches {
		fmt.Fprintf(&lines, "  %s  %s\n", prefixString(candidate.ID, 8), a.sessionLabel(candidate))
	}
	return "", fail(1, "ambiguous session prefix '%s' — matches:\n%srun `sessions ls`", idOrPrefix, lines.String())
}

func prefixString(value string, count int) string {
	if len(value) <= count {
		return value
	}
	return value[:count]
}

func (a *app) ageOf(timestamp int64) string {
	seconds := math.Max(0, math.Floor(float64(a.now().UnixMilli()-timestamp)/1000+0.5))
	if seconds < 60 {
		return strconv.FormatInt(int64(seconds), 10) + "s"
	}
	minutes := math.Floor(seconds/60 + 0.5)
	if minutes < 60 {
		return strconv.FormatInt(int64(minutes), 10) + "m"
	}
	hours := math.Floor(minutes/60 + 0.5)
	if hours < 48 {
		return strconv.FormatInt(int64(hours), 10) + "h"
	}
	days := math.Floor(hours/24 + 0.5)
	return strconv.FormatInt(int64(days), 10) + "d"
}

func (a *app) cmdLS(args []string) error {
	options, err := parseLSListOptions(args)
	if err != nil {
		return err
	}
	// --json selects a format, never a working set. It used to also force
	// closed sessions into the answer, so `sessions --json ls` returned every
	// record the daemon had ever seen and -a was a no-op there: an agent asking
	// the most common question there is — what is running? — got a pile of dead
	// sessions and no way to say otherwise. The flag now means the same thing
	// in both modes.
	records, err := a.fetchSessionRecords(options.includeClosed)
	if err != nil {
		return err
	}
	records = filterSessionRecords(records, func(value session) bool { return value.Kind != "lane" })
	if options.asideOnly {
		records = filterSessionRecords(records, func(value session) bool {
			return !value.Exited && value.SetAsideAt != nil
		})
	}
	if options.notAside {
		records = filterSessionRecords(records, func(value session) bool {
			return value.Exited || value.SetAsideAt == nil
		})
	}
	var scope ownershipScope
	if options.mine {
		scope, err = a.resolveOwnershipScope("", "")
		if err != nil {
			return err
		}
		records = filterSessionRecords(records, func(value session) bool {
			return matchesOwnership(value, scope, false)
		})
	}
	// The sessions the user pinned are the ones this list exists to find again.
	records = pinnedFirst(records)
	if a.wantJSON {
		return writeRawSessionRecords(a.stdout, records)
	}
	if scope.osUserFallback {
		writeOSUserScope(a.stdout, scope)
	}
	if len(records) == 0 {
		// An empty ls is the moment an agent is most likely to conclude that
		// the work it dispatched never happened, so name the two things this
		// view deliberately excludes rather than leaving it to the help text.
		_, err := io.WriteString(a.stdout, "(no sessions)\n"+
			"ls hides ended sessions and never lists lanes; `sessions list -a` shows every session and lane in any state\n")
		return err
	}
	showProfile := recordsHaveProfiles(records)
	showPin := recordsHavePins(records)
	header := []string{"ID", "NAME", "DESC", "TOOL"}
	if showProfile {
		header = append(header, "PROFILE")
	}
	if showPin {
		header = append(header, "PIN")
	}
	header = append(header, "CWD", "STATE", "SUMMARY", "AGE", "LAST-USER", "PID")
	rows := [][]string{header}
	for _, record := range records {
		value := record.value
		lastUser := "-"
		if value.LastUserMessageAt != nil && *value.LastUserMessageAt != 0 {
			lastUser = a.ageOf(*value.LastUserMessageAt)
		}
		row := []string{prefixString(value.ID, 8), compactSessionName(value.Name), compactDescription(value.Description), toolOfSession(value)}
		if showProfile {
			row = append(row, compactProfile(value.Profile))
		}
		if showPin {
			row = append(row, pinMark(value))
		}
		row = append(row, strings.Replace(value.Cwd, a.home, "~", 1), sessionState(value),
			compactSummary(value.LastSummary), a.ageOf(value.CreatedAt), lastUser, strconv.Itoa(value.PID))
		rows = append(rows, row)
	}
	return writePaddedRows(a.stdout, rows)
}

func jsLength(value string) int { return len(utf16.Encode([]rune(value))) }

var cursorForwardPattern = regexp.MustCompile("\\x1b\\[(\\d+)C")

func normalize(value string) string {
	return cursorForwardPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := cursorForwardPattern.FindStringSubmatch(match)
		count, _ := strconv.Atoi(parts[1])
		return strings.Repeat(" ", count)
	})
}

func cleanANSI(value string) string { return ansi.Strip(normalize(value)) }

func (a *app) cmdSnap(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions snap <id> [--raw]")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	text, err := a.getText("/api/sessions/" + escapeID(id) + "/snapshot")
	if err != nil {
		return err
	}
	if !contains(args, "--raw") {
		text = cleanANSI(text)
	}
	io.WriteString(a.stdout, text)
	if !strings.HasSuffix(text, "\n") {
		io.WriteString(a.stdout, "\n")
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
