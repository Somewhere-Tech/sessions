package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

type Session struct {
	ID                 string
	Name               string
	Description        string
	DescriptionSource  string
	ClaudeCustomTitle  string
	ClaudeAITitle      string
	CWD                string
	ConfigDir          string
	Tool               state.SessionTool
	Command            string
	Args               []string
	ConversationID     string
	ClaudeSessionID    string
	CreatedAt          int64
	LastActivityAt     int64
	CreatorKind        string
	CreatorID          string
	ReopenedAs         string
	ResumedFrom        string
	MovedToEndpoint    string
	MovedToSessionID   string
	MovedFromEndpoint  string
	MovedFromSessionID string
	OptOut             bool
}

type Resolver struct {
	ClaudeProjectsDir string
	CodexSessionsDir  string
	// RunnerStateDir locates Sessions' own transcript copies. The provider
	// prunes its transcripts on its own schedule, so when the original is
	// gone the mirror is the conversation. Empty disables the fallback.
	RunnerStateDir string
	Now            func() time.Time
}

func CollectSessions(live []state.SessionInfo, runnerStateDir string) []Session {
	collected := make(map[string]Session, len(live))
	for _, info := range live {
		if info.ID == "" {
			continue
		}
		lastActivity := max(info.CreatedAt, info.LastDataAt)
		if info.LastUserMessageAt != nil {
			lastActivity = max(lastActivity, *info.LastUserMessageAt)
		}
		collected[info.ID] = Session{
			ID: info.ID, Name: info.Name, CWD: info.Cwd, ConfigDir: info.ConfigDir, Tool: info.Tool,
			Description: info.Description, DescriptionSource: info.DescriptionSource,
			ClaudeCustomTitle: info.ClaudeCustomTitle, ClaudeAITitle: info.ClaudeAITitle,
			Command: info.Cmd, Args: append([]string(nil), info.Args...),
			ConversationID: info.ConversationID, ClaudeSessionID: info.ClaudeSessionID,
			CreatedAt: info.CreatedAt, LastActivityAt: lastActivity,
			CreatorKind: info.CreatorKind, CreatorID: info.CreatorID,
			ReopenedAs: info.ReopenedAs, ResumedFrom: info.ResumedFrom,
			MovedToEndpoint: info.MovedToEndpoint, MovedToSessionID: info.MovedToSessionID,
			MovedFromEndpoint: info.MovedFromEndpoint, MovedFromSessionID: info.MovedFromSessionID,
			OptOut: sessionOptedOut(runnerStateDir, info.ID),
		}
	}
	entries, _ := os.ReadDir(runnerStateDir)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		// Sidecar names are decided in one place, with a drift-guard test that
		// fails when a new Paths field adds a ".json" artifact. A local copy of
		// that rule would silently start collecting phantom sessions the next
		// time one is added.
		id, ok := state.RunnerIDFromMetadataName(name)
		if !ok {
			continue
		}
		if _, exists := collected[id]; exists {
			continue
		}
		path := filepath.Join(runnerStateDir, name)
		metadata, err := state.ReadRunnerMetadata(path)
		if err != nil || metadata.Info.ID == "" || metadata.Info.ID != id {
			continue
		}
		tool := classifySessionTool(metadata.Info.Cmd)
		lastActivity := metadata.Info.CreatedAt
		if info, err := entry.Info(); err == nil {
			lastActivity = max(lastActivity, info.ModTime().UnixMilli())
		}
		collected[id] = Session{
			ID: id, Name: metadata.Name, CWD: metadata.Info.Cwd, ConfigDir: metadata.ConfigDir, Tool: tool,
			Description: metadata.Description, DescriptionSource: metadata.DescriptionSource,
			Command: metadata.Info.Cmd, Args: append([]string(nil), metadata.Info.Args...),
			ConversationID: metadata.Info.ConversationID, ClaudeSessionID: metadata.Info.ClaudeSessionID,
			CreatedAt: metadata.Info.CreatedAt, LastActivityAt: lastActivity,
			OptOut: sessionOptedOut(runnerStateDir, id),
		}
	}
	result := make([]Session, 0, len(collected))
	for _, session := range collected {
		result = append(result, session)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r Resolver) Resolve(session Session) (path, tool string) {
	if session.OptOut {
		return "", ""
	}
	switch normalizedTool(session.Tool, session.Command) {
	case "claude":
		projects := r.ClaudeProjectsDir
		if session.ConfigDir != "" {
			projects = filepath.Join(session.ConfigDir, "projects")
		} else if projects == "" {
			resolved, err := watch.ClaudeProjectsDir()
			if err != nil {
				return "", ""
			}
			projects = resolved
		}
		launchID := extractClaudeSessionID(session.Args)
		if launchID == "" {
			launchID = session.ID
		}
		// The provider file wins whenever it resolves, so a session always
		// has exactly one transcript and nothing is counted twice.
		mirror := ""
		if r.RunnerStateDir != "" {
			mirror = watch.TranscriptMirrorPath(r.RunnerStateDir, session.ID)
		}
		resolution := watch.ResolveClaudeWithMirror(projects, session.CWD, launchID, mirror)
		return resolution.Path, "claude"
	case "codex":
		now := time.Now()
		if r.Now != nil {
			now = r.Now()
		}
		sessionsDir := r.CodexSessionsDir
		if session.ConfigDir != "" {
			sessionsDir = filepath.Join(session.ConfigDir, "sessions")
		}
		resolution := watch.ResolveCodexRolloutPath(watch.CodexResolveOptions{
			CWD: session.CWD, Args: session.Args,
			CreatedAt:   time.UnixMilli(session.CreatedAt),
			SessionsDir: sessionsDir, Now: now,
		})
		return resolution.Path, "codex"
	default:
		return "", ""
	}
}

func sessionOptedOut(runnerStateDir, id string) bool {
	if _, err := os.Stat(filepath.Join(runnerStateDir, id+".no-backup")); err == nil {
		return true
	}
	encoded, err := os.ReadFile(filepath.Join(runnerStateDir, id+".json"))
	if err != nil {
		return false
	}
	var flags struct {
		Backup       *bool `json:"backup"`
		BackupOptOut bool  `json:"backupOptOut"`
		NoBackup     bool  `json:"noBackup"`
	}
	if json.Unmarshal(encoded, &flags) != nil {
		return false
	}
	return flags.BackupOptOut || flags.NoBackup || (flags.Backup != nil && !*flags.Backup)
}

func classifySessionTool(command string) state.SessionTool {
	switch strings.ToLower(filepath.Base(command)) {
	case "claude":
		return state.ToolClaude
	case "codex":
		return state.ToolCodex
	default:
		return state.ToolTerminal
	}
}

func normalizedTool(tool state.SessionTool, command string) string {
	switch tool {
	case state.ToolClaude:
		return "claude"
	case state.ToolCodex:
		return "codex"
	}
	switch classifySessionTool(command) {
	case state.ToolClaude:
		return "claude"
	case state.ToolCodex:
		return "codex"
	default:
		return ""
	}
}

func extractClaudeSessionID(args []string) string {
	for index, argument := range args {
		for _, flag := range []string{"--session-id", "--resume"} {
			if argument == flag && index+1 < len(args) {
				return args[index+1]
			}
			if strings.HasPrefix(argument, flag+"=") {
				return strings.TrimPrefix(argument, flag+"=")
			}
		}
	}
	return ""
}
