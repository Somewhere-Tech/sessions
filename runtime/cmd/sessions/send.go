package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type sendEvidence struct {
	JSONLConfirmed      bool
	TextStillInComposer bool
	Working             bool
}

type sendDecision struct {
	Confidence string
	ExitCode   int
}

func decideSendConfirmation(evidence sendEvidence) sendDecision {
	if evidence.JSONLConfirmed {
		return sendDecision{Confidence: "confirmed", ExitCode: 0}
	}
	if !evidence.TextStillInComposer && evidence.Working {
		return sendDecision{Confidence: "accepted", ExitCode: 0}
	}
	if evidence.TextStillInComposer {
		return sendDecision{Confidence: "unconfirmed", ExitCode: 1}
	}
	return sendDecision{Confidence: "unconfirmed", ExitCode: 2}
}

type snapshotState struct {
	Kind        string
	Title       string
	Description string
}

type sendResult struct {
	Confirmed           *bool
	Confidence          string
	ExitCode            int
	Tool                string
	Text                string
	Reason              string
	TextStillInComposer *bool
	ComposerTail        string
	SnapshotState       *snapshotState
}

var (
	composerWorkingPattern = regexp.MustCompile(`(?i)(^|\n)\s*[•·∙]\s*Working\b`)
	pickerFooterPattern    = regexp.MustCompile(`Enter to select.*[↑↓].*to navigate`)
	numberedOptionPattern  = regexp.MustCompile(`^\s*([❯>]\s*)?\d+[\.)]\s+\S.+$`)
	selectionMarkerPattern = regexp.MustCompile(`^\s*[❯>]\s*\d+[\.)]\s+\S`)
	pickerLanguagePattern  = regexp.MustCompile(`(?i)\b(enter to select|navigate|select|choose|resume|continue|esc to cancel)\b`)
	trustPromptPattern     = regexp.MustCompile(`(?i)\b(do you trust|trust (this|the)|trusted (folder|directory|workspace|project)|trust the files|only grant access to directories you trust)\b`)
	trustContextPattern    = regexp.MustCompile(`(?i)\b(folder|directory|workspace|project|files in this)\b`)
	updateNoticePattern    = regexp.MustCompile(`(?i)\b(update available|new version|latest version|release notes|what'?s new|restart to update|install update|update now|press enter to continue|notice)\b`)
	blockingPromptPattern  = regexp.MustCompile(`(?i)\b(press enter|hit enter|continue\?|confirm|are you sure|allow|deny|approve|permission|yes/no|\[y/n\]|\(y/n\)|select|choose)\b`)
)

const (
	sendPollInterval = 300 * time.Millisecond
	maxEnterRetries  = 2
)

func getComposerLines(snapshot string) []string {
	if snapshot == "" {
		return nil
	}
	lines := strings.Split(cleanANSI(snapshot), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return lines
}

func cleanSnapshotText(snapshot string) string {
	return strings.ReplaceAll(cleanANSI(snapshot), "\r", "")
}

func snapshotTailLines(snapshot string, maximum int) []string {
	lines := strings.Split(cleanSnapshotText(snapshot), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maximum {
		lines = lines[len(lines)-maximum:]
	}
	for index := range lines {
		lines[index] = strings.TrimRightFunc(lines[index], unicode.IsSpace)
	}
	return lines
}

func hasNumberedPicker(snapshot string) bool {
	optionCount := 0
	hasSelectionMarker := false
	hasPickerLanguage := false
	for _, line := range snapshotTailLines(snapshot, 44) {
		trimmed := strings.TrimSpace(line)
		if pickerFooterPattern.MatchString(trimmed) {
			return true
		}
		if numberedOptionPattern.MatchString(trimmed) {
			optionCount++
		}
		if selectionMarkerPattern.MatchString(line) {
			hasSelectionMarker = true
		}
		if pickerLanguagePattern.MatchString(trimmed) {
			hasPickerLanguage = true
		}
	}
	return optionCount >= 2 && (hasSelectionMarker || hasPickerLanguage)
}

func classifySnapshotComposerState(snapshot string) snapshotState {
	tailText := strings.Join(snapshotTailLines(snapshot, 44), "\n")
	fullText := cleanSnapshotText(snapshot)
	if hasNumberedPicker(snapshot) {
		return snapshotState{
			Kind: "numbered-picker", Title: "Menu or picker is open",
			Description: "This session is showing a menu or picker, not a text box.",
		}
	}
	if trustPromptPattern.MatchString(tailText) && trustContextPattern.MatchString(tailText) {
		return snapshotState{
			Kind: "trust-prompt", Title: "Trust prompt is open",
			Description: "This session is asking whether to trust a folder or workspace.",
		}
	}
	if updateNoticePattern.MatchString(tailText) {
		return snapshotState{
			Kind: "update/notice-banner", Title: "Notice banner is open",
			Description: "This session is showing an update or notice banner before it will accept chat input.",
		}
	}
	if blockingPromptPattern.MatchString(tailText) && strings.TrimSpace(fullText) != "" {
		return snapshotState{
			Kind: "unknown-blocking", Title: "Interactive prompt is open",
			Description: "This session appears to be waiting on a terminal prompt instead of accepting a chat message.",
		}
	}
	return snapshotState{
		Kind: "normal-composer", Title: "Composer appears available",
		Description: "No blocking menu or prompt was detected in the terminal snapshot.",
	}
}

func isBlockingSnapshotState(state *snapshotState) bool {
	return state != nil && state.Kind != "normal-composer"
}

func snapshotStateCLILabel(state *snapshotState) string {
	if state == nil {
		return "an interactive prompt"
	}
	switch state.Kind {
	case "numbered-picker":
		return "a picker/menu"
	case "trust-prompt":
		return "a trust prompt"
	case "update/notice-banner":
		return "an update/notice banner"
	case "unknown-blocking":
		return "an interactive prompt"
	default:
		return "the normal composer"
	}
}

type eventMessage struct {
	Role      string           `json:"role"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []map[string]any `json:"tool_calls"`
}

func eventMessageOf(event map[string]any) (eventMessage, bool) {
	value, ok := event["message"]
	if !ok {
		return eventMessage{}, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return eventMessage{}, false
	}
	var message eventMessage
	if err := json.Unmarshal(encoded, &message); err != nil {
		return eventMessage{}, false
	}
	return message, true
}

func extractEventText(event map[string]any) string {
	message, ok := eventMessageOf(event)
	if !ok || len(message.Content) == 0 || string(message.Content) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return text
	}
	var blocks []map[string]any
	if json.Unmarshal(message.Content, &blocks) != nil {
		return ""
	}
	var result strings.Builder
	for _, block := range blocks {
		typeName, _ := block["type"].(string)
		if typeName != "text" && typeName != "input_text" && typeName != "output_text" {
			continue
		}
		if value, ok := block["text"].(string); ok {
			result.WriteString(value)
		}
	}
	return result.String()
}

func isRealUserEvent(event map[string]any) bool {
	if event["type"] != "user" {
		return false
	}
	message, ok := eventMessageOf(event)
	if !ok || message.Role != "user" {
		return false
	}
	var blocks []map[string]any
	if json.Unmarshal(message.Content, &blocks) == nil && len(blocks) > 0 {
		allToolResults := true
		for _, block := range blocks {
			if block["type"] != "tool_result" {
				allToolResults = false
				break
			}
		}
		if allToolResults {
			return false
		}
	}
	text := strings.TrimLeftFunc(extractEventText(event), unicode.IsSpace)
	return !strings.HasPrefix(text, "<") &&
		!strings.HasPrefix(text, "Caveat:") &&
		!strings.HasPrefix(text, "This session is being continued") &&
		!strings.HasPrefix(text, "[Request interrupted")
}

func extractEventToolCalls(event map[string]any) []string {
	message, ok := eventMessageOf(event)
	if !ok {
		return nil
	}
	result := make([]string, 0)
	var blocks []map[string]any
	if json.Unmarshal(message.Content, &blocks) == nil {
		for _, block := range blocks {
			typeName, _ := block["type"].(string)
			var name any
			switch typeName {
			case "tool_use", "server_tool_use":
				name = firstValue(block, "name", "tool_name", "id")
				if name == nil {
					name = "tool"
				}
			case "function_call":
				name = firstValue(block, "name", "call_id")
				if name == nil {
					name = "function_call"
				}
			}
			if name != nil {
				result = append(result, fmt.Sprint(name))
			}
		}
	}
	for _, call := range message.ToolCalls {
		name := any(nil)
		if function, ok := call["function"].(map[string]any); ok {
			name = function["name"]
		}
		if name == nil {
			name = firstValue(call, "name", "type")
		}
		if name == nil {
			name = "tool"
		}
		result = append(result, fmt.Sprint(name))
	}
	return result
}

func firstValue(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if candidate, ok := value[key]; ok && candidate != nil && fmt.Sprint(candidate) != "" {
			return candidate
		}
	}
	return nil
}

// submitComposer sends one logical message through the daemon's atomic submit
// boundary. Raw terminal keystrokes still use /input; message text and Enter
// must never be interleaved with another agent's concurrent submission.
func (a *app) submitComposer(inputPath, text, sourceSessionID string) error {
	headers := make(http.Header)
	if sourceSessionID == "" {
		sourceSessionID = a.api.creatorSession
	}
	if sourceSessionID != "" {
		headers.Set("X-Sessions-Creator-Session", sourceSessionID)
	}
	submitPath := strings.TrimSuffix(inputPath, "/input") + "/submit"
	return a.postJSONWithHeaders(submitPath, map[string]string{"data": text}, &map[string]any{}, 1, headers)
}

func (a *app) pressComposerEnter(inputPath string) error {
	return a.postJSON(inputPath, map[string]string{"data": "\r"}, &map[string]any{}, 1)
}

type eventsResponse struct {
	Events    []map[string]any `json:"events"`
	NextIndex int64            `json:"nextIndex"`
}

func (a *app) sendAndConfirm(id, text string, timeout time.Duration, noWait bool) (sendResult, error) {
	return a.sendAndConfirmFrom(id, text, timeout, noWait, "")
}

func (a *app) sendAndConfirmFrom(id, text string, timeout time.Duration, _ bool, sourceSessionID string) (sendResult, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	sessions, err := a.listSessions(false)
	if err != nil {
		return sendResult{}, err
	}
	var baseline *session
	for index := range sessions {
		if sessions[index].ID == id {
			baseline = &sessions[index]
			break
		}
	}
	if baseline == nil {
		return sendResult{}, fail(1, "%s", unknownSessionMessage(id))
	}
	tool := toolOfSession(*baseline)
	confirmable := isConfirmableTool(tool)
	baseTimestamp := int64(0)
	if baseline.LastUserMessageAt != nil {
		baseTimestamp = *baseline.LastUserMessageAt
	}
	baseNextIndex := int64(0)
	if confirmable {
		var events eventsResponse
		if err := a.getJSON("/api/sessions/"+escapeID(id)+"/events?tail=1", &events); err == nil {
			baseNextIndex = events.NextIndex
		}
	}
	inputPath := "/api/sessions/" + escapeID(id) + "/input"
	if err := a.submitComposer(inputPath, text, sourceSessionID); err != nil {
		return sendResult{}, err
	}
	if !confirmable {
		return sendResult{Confirmed: nil, Tool: tool}, nil
	}
	snippetSource := text
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			snippetSource = strings.TrimSpace(line)
			break
		}
	}
	snippet := prefixString(snippetSource, 25)
	start := a.now()
	enterRetries := 0
	for {
		current, err := a.listSessions(false)
		if err != nil {
			return sendResult{}, err
		}
		var currentSession *session
		for index := range current {
			if current[index].ID == id {
				currentSession = &current[index]
				break
			}
		}
		if currentSession == nil {
			confirmed := false
			return sendResult{
				Confirmed: &confirmed, Confidence: "unconfirmed", ExitCode: 1,
				Reason: "session-unreachable",
			}, nil
		}
		newTimestamp := int64(0)
		if currentSession.LastUserMessageAt != nil {
			newTimestamp = *currentSession.LastUserMessageAt
		}
		if newTimestamp > baseTimestamp {
			confirmedText := ""
			var events eventsResponse
			path := fmt.Sprintf("/api/sessions/%s/events?since=%d", escapeID(id), baseNextIndex)
			if err := a.getJSON(path, &events); err == nil {
				for index := len(events.Events) - 1; index >= 0; index-- {
					if isRealUserEvent(events.Events[index]) {
						confirmedText = extractEventText(events.Events[index])
						break
					}
				}
			}
			confirmed := true
			decision := decideSendConfirmation(sendEvidence{JSONLConfirmed: true})
			return sendResult{
				Confirmed: &confirmed, Confidence: decision.Confidence, ExitCode: decision.ExitCode,
				Text: confirmedText,
			}, nil
		}
		if a.now().Sub(start) >= timeout {
			snapshot, err := a.getText("/api/sessions/" + escapeID(id) + "/snapshot")
			if err != nil {
				return sendResult{}, err
			}
			composerLines := getComposerLines(snapshot)
			composerTail := strings.Join(composerLines, "\n")
			state := classifySnapshotComposerState(snapshot)
			textStillInComposer := snippet != "" && anyLineContains(composerLines, snippet)
			decision := decideSendConfirmation(sendEvidence{
				TextStillInComposer: textStillInComposer,
				Working:             currentSession.Working || composerWorkingPattern.MatchString(composerTail),
			})
			confirmed := decision.ExitCode == 0
			return sendResult{
				Confirmed: &confirmed, Confidence: decision.Confidence, ExitCode: decision.ExitCode,
				Reason: "timeout", TextStillInComposer: &textStillInComposer,
				ComposerTail: composerTail, SnapshotState: &state,
			}, nil
		}
		if enterRetries < maxEnterRetries {
			snapshot, err := a.getText("/api/sessions/" + escapeID(id) + "/snapshot")
			if err != nil {
				return sendResult{}, err
			}
			if snippet != "" && anyLineContains(getComposerLines(snapshot), snippet) {
				if err := a.pressComposerEnter(inputPath); err != nil {
					return sendResult{}, err
				}
				enterRetries++
			}
		}
		a.sleep(sendPollInterval)
	}
}

func anyLineContains(lines []string, snippet string) bool {
	for _, line := range lines {
		if strings.Contains(line, snippet) {
			return true
		}
	}
	return false
}

type sendJSONResult struct {
	Submitted               *bool   `json:"submitted"`
	Confidence              string  `json:"confidence"`
	Reason                  string  `json:"reason,omitempty"`
	Text                    string  `json:"text,omitempty"`
	Tool                    string  `json:"tool,omitempty"`
	SessionState            string  `json:"sessionState,omitempty"`
	SessionStateDescription string  `json:"sessionStateDescription,omitempty"`
	TextStillInComposer     *bool   `json:"textStillInComposer,omitempty"`
	ComposerTail            *string `json:"composerTail,omitempty"`
}

func (a *app) cmdSend(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions send <id> [--from session] [--no-wait] [--timeout Ns] [--file path] <text...>")
	}
	idArg := args[0]
	args = args[1:]
	noWait := removeFirst(&args, "--no-wait")
	timeout := 10 * time.Second
	if raw, present := pluck(&args, "--timeout"); present && raw != "" {
		var err error
		timeout, err = parseDuration(raw, 0)
		if err != nil {
			return err
		}
	}
	sourceID, hasSource := pluck(&args, "--from")
	if hasSource && sourceID == "" {
		return fail(1, "--from needs a source session")
	}
	filePath, hasFile := pluck(&args, "--file")
	if hasFile && filePath == "" {
		return fail(1, "--file needs a path")
	}
	// An unrecognized option in the leading position is a mistake, not message
	// text: silently typing `--json` into someone's agent session is worse than
	// refusing it. Later words are left alone so a message may still contain a
	// flag, and an explicit `--` sends the rest verbatim.
	if len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
		} else if strings.HasPrefix(args[0], "--") {
			return fail(1, "unknown send option %s — valid options are --from, --timeout, --no-wait, and --file; to send it as text use `sessions send <id> -- %s ...`",
				args[0], args[0])
		}
	}
	text := strings.Join(args, " ")
	if hasFile {
		if len(args) > 0 {
			return fail(1, "--file cannot be combined with inline text")
		}
		encoded, err := os.ReadFile(filePath)
		if err != nil {
			return fail(1, "could not read --file '%s': %s", filePath, err)
		}
		text = string(encoded)
	}
	if text == "" {
		return fail(1, "usage: sessions send <id> [--from session] [--no-wait] [--timeout Ns] [--file path] <text...>")
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	if hasSource {
		sourceID, err = a.resolveSessionID(sourceID)
		if err != nil {
			return err
		}
	}
	result, err := a.sendAndConfirmFrom(id, text, timeout, noWait, sourceID)
	if err != nil {
		return err
	}
	if result.Confirmed == nil {
		if !isConfirmableTool(result.Tool) {
			if a.wantJSON {
				return writeJSON(a.stdout, sendJSONResult{
					Submitted: nil, Confidence: "unconfirmed", Tool: result.Tool,
				}, false)
			}
			_, err := fmt.Fprintf(a.stdout, "sent (submission confirmation not available for tool: %s)\n", result.Tool)
			return err
		}
		return nil
	}
	if *result.Confirmed {
		if a.wantJSON {
			output := sendJSONResult{Submitted: boolPointer(true), Confidence: result.Confidence, Text: result.Text}
			if result.Confidence == "accepted" {
				output.Reason = "working-jsonl-pending"
			}
			return writeJSON(a.stdout, output, false)
		}
		if result.Confidence == "accepted" {
			_, err := io.WriteString(a.stdout, "accepted (working); JSONL confirmation pending\n")
			return err
		}
		_, err := io.WriteString(a.stdout, "delivered\n")
		return err
	}
	if a.wantJSON {
		output := sendJSONResult{
			Submitted: boolPointer(false), Confidence: "unconfirmed", Reason: result.Reason,
			TextStillInComposer: result.TextStillInComposer,
		}
		composerTail := result.ComposerTail
		output.ComposerTail = &composerTail
		if result.SnapshotState != nil {
			output.SessionState = result.SnapshotState.Kind
			output.SessionStateDescription = result.SnapshotState.Description
		}
		if err := writeJSON(a.stdout, output, false); err != nil {
			return err
		}
	} else if result.Reason == "session-unreachable" {
		io.WriteString(a.stderr, "sessions send: session exited or became unreachable before submission was confirmed\n")
	} else {
		fmt.Fprintf(a.stderr, "sessions send: could not confirm submission after %dms\n", timeout.Milliseconds())
		if isBlockingSnapshotState(result.SnapshotState) {
			fmt.Fprintf(a.stderr, "  session is at %s — not accepting a typed message; use `sessions keys` or the terminal view\n", snapshotStateCLILabel(result.SnapshotState))
			fmt.Fprintf(a.stderr, "  %s\n", result.SnapshotState.Description)
		} else if result.TextStillInComposer != nil && *result.TextStillInComposer {
			io.WriteString(a.stderr, "  the message is still in the composer (Enter did not submit)\n")
		} else {
			io.WriteString(a.stderr, "  sent but not confirmed — the session may still be starting; retry, or use `sessions wait` first\n")
			io.WriteString(a.stderr, "  message is no longer in the composer but no JSONL user event appeared yet\n")
			io.WriteString(a.stderr, "  (the tool may still be picking it up, or the session may be confused)\n")
		}
		if result.ComposerTail != "" {
			fmt.Fprintf(a.stderr, "  inspect the terminal with `sessions snap %s` if the provider is showing a picker or prompt\n", id)
		}
	}
	return status(result.ExitCode)
}

func boolPointer(value bool) *bool { return &value }
