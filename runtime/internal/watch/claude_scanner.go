package watch

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	claudePreviewBytes = 16 * 1024
	claudeTitleBytes   = 4 * 1024 * 1024
)

// ResumableSession is the exact metadata shape returned by the TypeScript
// /api/claude-sessions route.
type ResumableSession struct {
	SessionID          string         `json:"sessionId"`
	Tool               string         `json:"tool"`
	External           bool           `json:"external,omitempty"`
	Origin             string         `json:"origin,omitempty"`
	Title              string         `json:"title,omitempty"`
	HistoryID          string         `json:"historyId,omitempty"`
	PromptHistoryOnly  bool           `json:"promptHistoryOnly,omitempty"`
	TranscriptRecovery bool           `json:"transcriptRecovery,omitempty"`
	Runs               []ResumableRun `json:"runs,omitempty"`
	Cwd                string         `json:"cwd"`
	ModifiedAt         float64        `json:"modifiedAt"`
	FirstUserMessage   string         `json:"firstUserMessage"`
	SizeBytes          int64          `json:"sizeBytes"`
	SourcePath         string         `json:"-"`

	// Surface is where the conversation was started from, read from the same
	// provider metadata this scan already parses. Nil means the provider
	// recorded nothing, which a caller must not render as a blank surface.
	Surface *ConversationSurface `json:"surface,omitempty"`
}

type ResumableRun struct {
	SessionID          string `json:"sessionId"`
	Name               string `json:"name,omitempty"`
	StartedAt          int64  `json:"startedAt"`
	LastActivityAt     int64  `json:"lastActivityAt"`
	Machine            string `json:"machine,omitempty"`
	ReopenedAs         string `json:"reopenedAs,omitempty"`
	ResumedFrom        string `json:"resumedFrom,omitempty"`
	MovedToEndpoint    string `json:"movedToEndpoint,omitempty"`
	MovedToSessionID   string `json:"movedToSessionId,omitempty"`
	MovedFromEndpoint  string `json:"movedFromEndpoint,omitempty"`
	MovedFromSessionID string `json:"movedFromSessionId,omitempty"`
}

type resumableTask struct {
	path      string
	cwd       string
	sessionID string
}

type resumableResult struct {
	session ResumableSession
	ok      bool
}

// ScanResumableSessions walks ~/.claude/projects without modifying it and
// returns Claude conversations newest-first. HOME is resolved at call time so
// callers can isolate the scan with a fixture HOME.
func ScanResumableSessions() []ResumableSession {
	projectsDir, err := ClaudeProjectsDir()
	if err != nil {
		return []ResumableSession{}
	}
	return scanResumableClaudeSessions(projectsDir)
}

func scanResumableClaudeSessions(projectsDir string) []ResumableSession {
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return []ResumableSession{}
	}

	tasks := make([]resumableTask, 0)
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(projectsDir, project.Name())
		files, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		cwd := claudeProjectDirCWD(projectDir)
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			tasks = append(tasks, resumableTask{
				path:      filepath.Join(projectDir, file.Name()),
				cwd:       cwd,
				sessionID: strings.TrimSuffix(file.Name(), ".jsonl"),
			})
		}
	}

	out := make([]ResumableSession, 0, len(tasks))
	const pool = 16
	for start := 0; start < len(tasks); start += pool {
		end := min(start+pool, len(tasks))
		results := make([]resumableResult, end-start)
		done := make(chan struct{}, len(results))
		for i, task := range tasks[start:end] {
			go func(index int, task resumableTask) {
				defer func() { done <- struct{}{} }()
				info, err := os.Stat(task.path)
				if err != nil {
					return
				}
				session := ResumableSession{
					SessionID:        task.sessionID,
					Tool:             "claude",
					Origin:           "Claude Code",
					Title:            claudeConversationTitle(task.path),
					Cwd:              task.cwd,
					ModifiedAt:       float64(info.ModTime().UnixNano()) / 1_000_000,
					FirstUserMessage: firstUserMessageOf(task.path),
					SizeBytes:        info.Size(),
					SourcePath:       task.path,
				}
				// One bounded probe answers both questions: which directory this
				// conversation actually ran in, and which surface started it.
				// What the transcript recorded about itself outranks anything
				// derivable from the directory name.
				if facts, ok := probeClaudeTranscript(task.path); ok {
					if facts.CWD != "" {
						session.Cwd = facts.CWD
					}
					if surface := ClaudeSurface(
						facts.Entrypoint, facts.PromptSource, facts.Version, facts.Sidechain,
					); surface.Known() {
						session.Surface = &surface
					}
				}
				results[index] = resumableResult{ok: true, session: session}
			}(i, task)
		}
		for range results {
			<-done
		}
		for _, result := range results {
			if result.ok {
				out = append(out, result.session)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ModifiedAt > out[j].ModifiedAt
	})
	return out
}

// claudeProjectDirCWD is the bucket-level fallback for a conversation whose own
// records carried no working directory.
//
// The project-directory name is a lossy encoding -- Claude folds every
// non-alphanumeric byte to a dash, so /Users/u/a-b, /Users/u/a/b, /Users/u/a.b
// and /Users/u/a_b all land in -Users-u-a-b -- and inverting it is a guess.
// Presenting that guess as the conversation's directory is worse than printing
// nothing: the directory column is the one a person uses to recognise which
// conversation is theirs, and this scanner was printing /Users/uzair/pretty/PTY
// for conversations that ran in /Users/uzair/pretty-PTY. A path that does not
// exist tells the reader nothing and costs them their trust in the column.
//
// So: the inversion is accepted only when it names a directory that actually
// exists, which is what separates a plausible reading from a fabricated one.
// Otherwise the answer is nothing at all. This is the rule recovery/adopt.go
// already holds itself to for the same encoding and the same reason.
//
// The per-conversation caller does better still -- a transcript that recorded
// its own cwd is read directly, and this is only consulted when none did.
func claudeProjectDirCWD(projectDir string) string {
	decoded := decodeClaudeProjectName(filepath.Base(projectDir))
	if decoded == "" {
		return ""
	}
	if info, err := os.Stat(decoded); err != nil || !info.IsDir() {
		return ""
	}
	return decoded
}

func decodeClaudeProjectName(encoded string) string {
	separator := string(filepath.Separator)
	if len(encoded) >= 3 &&
		((encoded[0] >= 'A' && encoded[0] <= 'Z') || (encoded[0] >= 'a' && encoded[0] <= 'z')) &&
		encoded[1:3] == "--" {
		return string(encoded[0]) + ":" + separator + strings.ReplaceAll(encoded[3:], "-", separator)
	}
	return strings.ReplaceAll(encoded, "-", separator)
}

// ScanResumableConversations returns provider conversations which can be
// safely bound through the audited recovery/adopt boundary. It reads only
// local provider metadata and a bounded first-message preview; no transcript
// content leaves this Mac.
func ScanResumableConversations() []ResumableSession {
	return ScanResumableConversationsIn("", "")
}

// ScanResumableConversationsIn is the explicit-root form used by authenticated
// local history surfaces and isolated tests. SourcePath is intentionally
// excluded from JSON so provider store locations never enter a WebView.
func ScanResumableConversationsIn(claudeProjectsDir, codexSessionsDir string) []ResumableSession {
	var out []ResumableSession
	if claudeProjectsDir == "" {
		out = append(out, ScanResumableSessions()...)
	} else {
		out = append(out, scanResumableClaudeSessions(claudeProjectsDir)...)
	}
	root := resolveCodexRoot(codexSessionsDir)
	for _, candidate := range listRolloutsRecursive(root) {
		if session, ok := resumableCodexConversation(candidate.path, candidate.modTime); ok {
			out = append(out, session)
		}
	}

	// A provider may create more than one rollout file when the same logical
	// conversation is resumed. Keep only the newest physical source per
	// provider identity so the picker never presents duplicate cards.
	byIdentity := make(map[string]ResumableSession, len(out))
	for _, session := range out {
		key := session.Tool + ":" + session.SessionID
		current, exists := byIdentity[key]
		if !exists || session.ModifiedAt > current.ModifiedAt {
			byIdentity[key] = session
		}
	}
	out = out[:0]
	for _, session := range byIdentity {
		out = append(out, session)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ModifiedAt != out[j].ModifiedAt {
			return out[i].ModifiedAt > out[j].ModifiedAt
		}
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

const codexResumePreviewBytes = 512 * 1024

func resumableCodexConversation(path string, modified time.Time) (ResumableSession, bool) {
	file, err := os.Open(path)
	if err != nil {
		return ResumableSession{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ResumableSession{}, false
	}
	reader := bufio.NewReader(io.LimitReader(file, codexResumePreviewBytes))
	session := ResumableSession{
		Tool: "codex", Origin: "Codex", ModifiedAt: float64(modified.UnixNano()) / 1_000_000,
		SizeBytes: info.Size(), SourcePath: path,
	}
	lineIndex := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var decoded map[string]any
			if json.Unmarshal(line, &decoded) == nil {
				if decoded["type"] == "session_meta" && session.SessionID == "" {
					if payload, ok := decoded["payload"].(map[string]any); ok {
						session.SessionID, _ = payload["id"].(string)
						session.Cwd, _ = payload["cwd"].(string)
						if originator, ok := payload["originator"].(string); ok && strings.TrimSpace(originator) != "" {
							session.Origin = originator
						}
						if codexSubagentSource(payload) {
							session.Origin = "Codex child agent"
						}
						// Same payload, no second read: provenance comes out of
						// the session_meta line already decoded above.
						if surface := CodexSurface(payload); surface.Known() {
							session.Surface = &surface
						}
					}
				}
				if session.FirstUserMessage == "" {
					normalized := NormalizeCodexRolloutLine(decoded, CodexNormalizeContext{
						RolloutBasename: filepath.Base(path), LineIndex: lineIndex,
					})
					for _, event := range normalized.Events {
						if event["type"] != "user" {
							continue
						}
						message, _ := event["message"].(map[string]any)
						if text := normalizedMessageText(message); text != "" {
							session.FirstUserMessage = previewText(text)
							break
						}
					}
				}
			}
			lineIndex++
		}
		if readErr != nil {
			break
		}
	}
	if session.SessionID == "" || session.Cwd == "" {
		return ResumableSession{}, false
	}
	if session.FirstUserMessage == "" && session.Origin == "Codex child agent" {
		session.FirstUserMessage = "Delegated child-agent work"
	}
	return session, true
}

func claudeConversationTitle(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	offset := max(int64(0), info.Size()-claudeTitleBytes)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	reader := bufio.NewReader(io.LimitReader(file, claudeTitleBytes))
	if offset > 0 {
		_, _ = reader.ReadBytes('\n')
	}
	var customTitle, aiTitle string
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event map[string]any
			if json.Unmarshal(line, &event) == nil {
				switch event["type"] {
				case "custom-title":
					if value, ok := event["customTitle"].(string); ok && strings.TrimSpace(value) != "" {
						customTitle = strings.TrimSpace(value)
					}
				case "ai-title":
					if value, ok := event["aiTitle"].(string); ok && strings.TrimSpace(value) != "" {
						aiTitle = strings.TrimSpace(value)
					}
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if customTitle != "" {
		return customTitle
	}
	return aiTitle
}

func codexSubagentSource(payload map[string]any) bool {
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = source["subagent"].(map[string]any)
	return ok
}

func normalizedMessageText(message map[string]any) string {
	blocks, _ := message["content"].([]any)
	parts := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" && !isCodexInjectedUserText(text) {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func isCodexInjectedUserText(value string) bool {
	trimmed := strings.TrimSpace(value)
	for _, prefix := range []string{
		"<recommended_plugins>", "# AGENTS.md instructions", "<environment_context>",
		"<permissions instructions>", "<app-context>", "<apps_instructions>",
		"<plugins_instructions>", "<skills_instructions>", "<collaboration_mode>",
		"<system-reminder>", "<local-command-", "<command-name>",
		"<command-message>", "<command-args>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func firstUserMessageOf(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	buffer := make([]byte, claudePreviewBytes)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return ""
	}
	for _, line := range strings.Split(string(buffer[:n]), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil || event["type"] != "user" {
			continue
		}
		message, ok := event["message"].(map[string]any)
		if !ok {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			return previewText(content)
		case []any:
			for _, raw := range content {
				block, ok := raw.(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					return previewText(text)
				}
			}
		}
	}
	return ""
}

func previewText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 200 {
		value = string(runes[:200])
	}
	return value
}
