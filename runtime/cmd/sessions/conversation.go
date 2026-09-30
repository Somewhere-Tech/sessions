package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type messageTurn struct {
	Role      string         `json:"role"`
	Text      string         `json:"text"`
	Timestamp any            `json:"timestamp"`
	ToolCalls []string       `json:"toolCalls,omitempty"`
	Author    *messageAuthor `json:"author,omitempty"`
	Subtype   string         `json:"subtype,omitempty"`
	Approval  map[string]any `json:"approval,omitempty"`
	index     int
}

type messageAuthor struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Client string `json:"client"`
}

func eventAuthor(event map[string]any) *messageAuthor {
	value, ok := event["author"].(map[string]any)
	if !ok {
		return nil
	}
	author := &messageAuthor{
		Kind: fmt.Sprint(value["kind"]), ID: fmt.Sprint(value["id"]),
		Name: fmt.Sprint(value["name"]), Client: fmt.Sprint(value["client"]),
	}
	if author.Kind != "session" || author.ID == "" || author.Name == "" {
		return nil
	}
	return author
}

func eventRole(event map[string]any) string {
	if isRealUserEvent(event) {
		return "user"
	}
	if event["type"] == "assistant" {
		if message, ok := eventMessageOf(event); ok && message.Role == "assistant" {
			// Codex emits normalized usage and task-complete metadata as
			// assistant records with empty content. They delimit a turn but are
			// not messages; selecting one would make `sessions last` hide the
			// actual text reply that immediately precedes it.
			if extractEventText(event) != "" || len(extractEventToolCalls(event)) > 0 {
				return "assistant"
			}
		}
	}
	return ""
}

func eventTimestamp(event map[string]any) any {
	if value, ok := event["timestamp"]; ok {
		return value
	}
	return nil
}

func approvalAuditTurn(event map[string]any) (messageTurn, bool) {
	if event["type"] != "system" {
		return messageTurn{}, false
	}
	subtype, _ := event["subtype"].(string)
	if subtype != "approval_requested" && subtype != "approval_resolved" {
		return messageTurn{}, false
	}
	approval, _ := event["approval"].(map[string]any)
	detail := ""
	if subtype == "approval_requested" {
		detail, _ = approval["summary"].(string)
	} else {
		detail, _ = approval["decision"].(string)
		if by, _ := approval["by"].(string); by != "" {
			detail += " by " + by
		}
	}
	text := subtype
	if detail != "" {
		text += ": " + detail
	}
	return messageTurn{
		Role: "system", Text: text, Timestamp: eventTimestamp(event),
		Subtype: subtype, Approval: approval,
	}, true
}

func providerFaultTurn(event map[string]any) (messageTurn, bool) {
	if event["type"] != "system" || event["subtype"] != "provider_fault" {
		return messageTurn{}, false
	}
	detail, _ := event["detail"].(string)
	if strings.TrimSpace(detail) == "" {
		return messageTurn{}, false
	}
	return messageTurn{Role: "error", Text: detail, Timestamp: eventTimestamp(event), Subtype: "provider_fault"}, true
}

func (a *app) cmdLast(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions last <id> [--role user|assistant] [-n N]")
	}
	idArg := args[0]
	args = args[1:]
	role := ""
	if value, present := pluck(&args, "--role"); present && value != "" {
		role = strings.ToLower(value)
		if role != "user" && role != "assistant" {
			return fail(1, "--role must be \"user\" or \"assistant\"")
		}
	}
	n := 1
	if value, present := pluck(&args, "-n"); present && value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return fail(1, "-n must be a positive integer")
		}
		n = parsed
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	tail := n * 20
	if tail < 100 {
		tail = 100
	}
	var response eventsResponse
	if err := a.getJSON(fmt.Sprintf("/api/sessions/%s/events?tail=%d", escapeID(id), tail), &response); err != nil {
		return err
	}
	matched := make([]messageTurn, 0)
	for index, event := range response.Events {
		eventRole := eventRole(event)
		if eventRole == "" || (role != "" && eventRole != role) {
			continue
		}
		matched = append(matched, messageTurn{
			Role: eventRole, Text: extractEventText(event), Timestamp: eventTimestamp(event),
			Author: eventAuthor(event), index: index,
		})
	}
	lastOfRole := func(want string) []messageTurn {
		selected := make([]messageTurn, 0)
		for _, turn := range matched {
			if turn.Role == want {
				selected = append(selected, turn)
			}
		}
		if len(selected) > n {
			selected = selected[len(selected)-n:]
		}
		return selected
	}
	toShow := make([]messageTurn, 0)
	if role != "" {
		toShow = lastOfRole(role)
	} else {
		toShow = append(lastOfRole("user"), lastOfRole("assistant")...)
		for i := 1; i < len(toShow); i++ {
			for j := i; j > 0 && toShow[j].index < toShow[j-1].index; j-- {
				toShow[j], toShow[j-1] = toShow[j-1], toShow[j]
			}
		}
	}
	if a.wantJSON {
		for index := range toShow {
			toShow[index].index = 0
		}
		return writeJSON(a.stdout, toShow, true)
	}
	if len(toShow) == 0 {
		_, err := io.WriteString(a.stdout, "(no messages)\n")
		return err
	}
	for _, turn := range toShow {
		header := "[" + turn.Role + "]"
		if turn.Author != nil {
			header = "[" + turn.Author.Name + " · via Sessions]"
		}
		if parsed, ok := parseEventTime(turn.Timestamp); ok {
			header += "  " + a.ageOf(parsed.UnixMilli()) + " ago"
		}
		fmt.Fprintln(a.stdout, header)
		text := turn.Text
		if text == "" {
			text = "(empty)"
		}
		text = trimEndJS(text)
		fmt.Fprintf(a.stdout, "%s\n\n", text)
	}
	return nil
}

func parseEventTime(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || text == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func (a *app) cmdTranscript(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions transcript <id>")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	return a.writeSessionTranscript(id)
}

func (a *app) writeSessionTranscript(id string) error {
	var response eventsResponse
	if err := a.getJSON("/api/sessions/"+escapeID(id)+"/events", &response); err != nil {
		return err
	}
	turns := make([]messageTurn, 0)
	for _, event := range response.Events {
		if fault, ok := providerFaultTurn(event); ok {
			turns = append(turns, fault)
			continue
		}
		if audit, ok := approvalAuditTurn(event); ok {
			turns = append(turns, audit)
			continue
		}
		role := eventRole(event)
		if role == "" {
			continue
		}
		text := extractEventText(event)
		var calls []string
		if role == "assistant" {
			calls = extractEventToolCalls(event)
		}
		if text == "" && len(calls) == 0 {
			continue
		}
		turns = append(turns, messageTurn{
			Role: role, Text: text, Timestamp: eventTimestamp(event),
			ToolCalls: calls, Author: eventAuthor(event),
		})
	}
	turns, err := a.appendSessionFault(turns, id)
	if err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, turns, true)
	}
	if len(turns) == 0 {
		message := "(no messages)\n"
		sessions, listErr := a.listSessions(true)
		if listErr != nil {
			return listErr
		}
		for _, current := range sessions {
			if current.ID != id || toolOfSession(current) != "codex" || current.Kind == state.KindCodexAppServer {
				continue
			}
			if current.Exited {
				message = "(Codex did not publish a conversation transcript before this terminal session ended)\n"
			} else {
				message = "(waiting for Codex to publish its conversation transcript)\n"
			}
			break
		}
		_, err := io.WriteString(a.stdout, message)
		return err
	}
	for index, turn := range turns {
		label := turn.Role
		if turn.Author != nil {
			label = turn.Author.Name + " · via Sessions"
		}
		fmt.Fprintf(a.stdout, "[%s]\n", label)
		body := trimEndJS(turn.Text)
		if body != "" {
			fmt.Fprintln(a.stdout, body)
		}
		for _, call := range turn.ToolCalls {
			fmt.Fprintf(a.stdout, "⚙ %s\n", call)
		}
		if index != len(turns)-1 {
			io.WriteString(a.stdout, "\n")
		}
	}
	return nil
}

func (a *app) appendSessionFault(turns []messageTurn, id string) ([]messageTurn, error) {
	for _, turn := range turns {
		if turn.Subtype == "provider_fault" {
			return turns, nil
		}
	}
	sessions, err := a.listSessions(true)
	if err != nil {
		return nil, err
	}
	for _, current := range sessions {
		if current.ID == id && current.FailureDetail != "" {
			turns = append(turns, messageTurn{
				Role: "error", Text: current.FailureDetail,
				Timestamp: time.UnixMilli(current.FailureAt).UTC().Format(time.RFC3339Nano),
				Subtype:   "provider_fault",
			})
			break
		}
	}
	return turns, nil
}

// askJSONResult is what ask returns once the message has been delivered and
// only the reply is outstanding. The fields it shares with sendJSONResult —
// submitted, confidence, reason — keep their meaning, so a caller that already
// parses send's answer can read this one too; reply is always present so its
// absence is a value rather than a missing key.
type askJSONResult struct {
	OperationID string    `json:"operation_id,omitempty"`
	Submitted   bool      `json:"submitted"`
	Confidence  string    `json:"confidence"`
	Reason      string    `json:"reason,omitempty"`
	Working     *bool     `json:"working,omitempty"`
	Reply       *askReply `json:"reply"`
}

type askReply struct {
	Text      string `json:"text"`
	Timestamp any    `json:"timestamp"`
}

func (a *app) cmdAsk(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions ask <id> [--timeout Ns] [--idle Ns] [--wait-timeout Ns] <text...>")
	}
	idArg := args[0]
	args = args[1:]
	timeout := 10 * time.Second
	idle := 2 * time.Second
	waitTimeout := 120 * time.Second
	var err error
	if raw, present := pluck(&args, "--timeout"); present && raw != "" {
		timeout, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if raw, present := pluck(&args, "--idle"); present && raw != "" {
		idle, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	if raw, present := pluck(&args, "--wait-timeout"); present && raw != "" {
		waitTimeout, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	text := strings.Join(args, " ")
	if text == "" {
		return fail(1, "usage: sessions ask <id> [options] <text...>")
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	result, err := a.sendAndConfirm(id, text, timeout, false)
	if err != nil {
		return err
	}
	if result.Confirmed == nil {
		// ask and send share sendAndConfirm, so they answer the same question
		// with the same document. This branch used to omit confidence, and
		// under --json it returned the document with status 0 for a failure
		// that exits 1 in prose mode — an agent that switched on --json was
		// told the ask had succeeded.
		if a.wantJSON {
			if err := writeJSON(a.stdout, sendJSONResult{
				OperationID: result.OperationID, Submitted: nil, Confidence: "unconfirmed", Tool: result.Tool,
			}, false); err != nil {
				return err
			}
			return status(1)
		}
		fmt.Fprintf(a.stderr, "sessions ask: submission confirmation not available for tool '%s'\n", result.Tool)
		io.WriteString(a.stderr, "  use 'sessions send' + 'sessions wait' instead\n")
		return status(1)
	}
	if !*result.Confirmed {
		if a.wantJSON {
			composerTail := result.ComposerTail
			output := sendJSONResult{
				OperationID: result.OperationID, Submitted: boolPointer(false), Confidence: result.Confidence, Reason: result.Reason,
				TextStillInComposer: result.TextStillInComposer, ComposerTail: &composerTail,
			}
			if result.SnapshotState != nil {
				output.SessionState = result.SnapshotState.Kind
				output.SessionStateDescription = result.SnapshotState.Description
			}
			if err := writeJSON(a.stdout, output, false); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(a.stderr, "sessions ask: message not confirmed submitted (%s)\n", result.Reason)
			if isBlockingSnapshotState(result.SnapshotState) {
				fmt.Fprintf(a.stderr, "  session is at %s — not accepting a typed message; use `sessions keys` or the terminal view\n", snapshotStateCLILabel(result.SnapshotState))
				fmt.Fprintf(a.stderr, "  %s\n", result.SnapshotState.Description)
			} else {
				io.WriteString(a.stderr, "  the session may still be starting; retry, or use `sessions wait` first\n")
			}
			if result.ComposerTail != "" {
				fmt.Fprintln(a.stderr, result.ComposerTail)
			}
		}
		// send distinguishes "the text is still sitting in the composer" (1)
		// from "it left the composer and nothing acknowledged it" (2). ask
		// collapsed both into 1 despite reading the same evidence.
		return status(result.ExitCode)
	}
	a.sleep(500 * time.Millisecond)
	waitStart := a.now()
	poll := idle / 4
	if poll < 100*time.Millisecond {
		poll = 100 * time.Millisecond
	}
	if poll > 500*time.Millisecond {
		poll = 500 * time.Millisecond
	}
	var notWorkingSince time.Time
	seenWorking := false
	for {
		sessions, err := a.listSessions(false)
		if err != nil {
			return err
		}
		var current *session
		for index := range sessions {
			if sessions[index].ID == id {
				current = &sessions[index]
				break
			}
		}
		if current == nil {
			break
		}
		if current.Working {
			seenWorking = true
			notWorkingSince = time.Time{}
		} else if notWorkingSince.IsZero() {
			notWorkingSince = a.now()
		}
		idleFor := time.Duration(0)
		if !notWorkingSince.IsZero() {
			idleFor = a.now().Sub(notWorkingSince)
		}
		elapsed := a.now().Sub(waitStart)
		if (seenWorking || elapsed > 3*time.Second) && idleFor >= idle {
			break
		}
		if elapsed >= waitTimeout {
			if a.wantJSON {
				working := current.Working
				if err := writeJSON(a.stdout, askJSONResult{
					OperationID: result.OperationID, Submitted: true, Confidence: result.Confidence,
					Reason: "wait-timeout", Working: &working,
				}, false); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(a.stderr, "sessions ask: timed out waiting for reply after %dms\n", waitTimeout.Milliseconds())
			}
			// The message was delivered and the target may still be working,
			// which is exactly what exit 3 means everywhere else in Sessions.
			// It used to report 1, the code reserved for a usage mistake.
			return status(exitWaitTimeout)
		}
		a.sleep(poll)
	}
	var events eventsResponse
	if err := a.getJSON("/api/sessions/"+escapeID(id)+"/events?tail=50", &events); err != nil {
		return err
	}
	var last map[string]any
	for _, event := range events.Events {
		if eventRole(event) == "assistant" {
			last = event
		}
	}
	if last == nil {
		if a.wantJSON {
			return writeJSON(a.stdout, askJSONResult{
				OperationID: result.OperationID, Submitted: true, Confidence: result.Confidence,
			}, false)
		}
		_, err := io.WriteString(a.stdout, "(no assistant reply found)\n")
		return err
	}
	replyText := extractEventText(last)
	if a.wantJSON {
		return writeJSON(a.stdout, askJSONResult{
			OperationID: result.OperationID, Submitted: true, Confidence: result.Confidence,
			Reply: &askReply{Text: replyText, Timestamp: eventTimestamp(last)},
		}, false)
	}
	io.WriteString(a.stdout, trimEndJS(replyText))
	if replyText != "" && !strings.HasSuffix(replyText, "\n") {
		io.WriteString(a.stdout, "\n")
	}
	return nil
}

func trimEndJS(value string) string {
	return strings.TrimRightFunc(value, unicode.IsSpace)
}
