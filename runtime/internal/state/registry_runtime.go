package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
)

func (r *Registry) SetTerminalObservers(
	onRunnerExit func(string, proto.ExitEvent),
	onReaped func(string),
) {
	r.onRunnerExit = onRunnerExit
	r.onReaped = onReaped
}

func (r *Registry) Discover(ctx context.Context) error {
	if r.launcher == nil {
		return errors.New("runner launcher is unavailable")
	}
	r.setDiscovering(true)
	defer r.setDiscovering(false)

	ids, err := RunnerArtifactIDs(r.config.RunnerStateDir)
	if err != nil {
		return fmt.Errorf("read runner state directory: %w", err)
	}
	var attachErrors []error
	for _, id := range ids {
		metadata, err := readRunnerMetadata(filepath.Join(r.config.RunnerStateDir, id+".json"))
		if err != nil {
			attachErrors = append(attachErrors, fmt.Errorf("discover %s: %w", id, err))
			continue
		}
		if metadata.Info.ID != id {
			attachErrors = append(attachErrors, fmt.Errorf("discover %s: metadata id is %q", id, metadata.Info.ID))
			continue
		}
		r.mu.RLock()
		existing, exists := r.sessions[id]
		r.mu.RUnlock()
		if exists && !existing.Info().Unreachable {
			continue
		}
		runner, err := r.launcher.Attach(ctx, metadata.Info)
		if err != nil {
			// Sessions are sacred: an unreachable socket is never deleted here.
			attachErrors = append(attachErrors, fmt.Errorf("discover %s: %w", id, err))
			continue
		}
		if actual := runner.Info().ID; actual != id {
			attachErrors = append(attachErrors, fmt.Errorf("discover %s: runner id is %q", id, actual))
			continue
		}
		if _, err := r.RegisterMetadata(ctx, runner, metadata, ""); err != nil {
			attachErrors = append(attachErrors, fmt.Errorf("discover %s: %w", id, err))
		}
	}
	return errors.Join(attachErrors...)
}

func (r *Registry) List(includeExited bool) []SessionInfo {
	r.mu.RLock()
	sessions := make([]*Session, 0, len(r.order))
	for _, id := range r.order {
		if session := r.sessions[id]; session != nil {
			sessions = append(sessions, session)
		}
	}
	r.mu.RUnlock()
	result := make([]SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		info := session.Info()
		if includeExited || !info.Exited {
			result = append(result, info)
		}
	}
	return result
}

func (r *Registry) removeOrderLocked(id string) {
	for index, existing := range r.order {
		if existing == id {
			r.order = append(r.order[:index], r.order[index+1:]...)
			return
		}
	}
}

func (r *Registry) Get(id string) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	return session, ok
}

// Kill sends one runner KILL frame. The higher-level session manager applies
// the mass-kill policy before calling this low-level operation.
func (r *Registry) Kill(ctx context.Context, id string, _ bool) bool {
	return r.RequestKill(ctx, id, false) == nil
}

// RequestKill is the low-level runner operation. The session manager records
// the durable user-kill tombstone before invoking it.
func (r *Registry) RequestKill(ctx context.Context, id string, _ bool) error {
	session, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	return session.RequestKill(ctx)
}

func (r *Registry) Input(ctx context.Context, id, data string) bool {
	session, ok := r.Get(id)
	return ok && session.Input(ctx, data)
}

func (r *Registry) ConfigureModel(ctx context.Context, id, model, effort string) (SessionInfo, error) {
	session, ok := r.Get(id)
	if !ok {
		return SessionInfo{}, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	if err := session.ConfigureModel(ctx, model, effort); err != nil {
		return SessionInfo{}, err
	}
	return session.Info(), nil
}

func (r *Registry) DeepDiagnostics() []map[string]any {
	list := r.List(true)
	now := time.Now().UnixMilli()
	result := make([]map[string]any, 0, len(list))
	for _, info := range list {
		// List(true) includes exited sessions, which the post-exit grace timer
		// removes from the registry 30 seconds later. A diagnostics request
		// racing that timer must report the session, not panic on a nil
		// receiver.
		claudeEvents := int64(0)
		if session, ok := r.Get(info.ID); ok {
			claudeEvents = session.ClaudeEventCount()
		}
		result = append(result, map[string]any{
			"id":            info.ID,
			"tool":          info.Tool,
			"cols":          info.Cols,
			"rows":          info.Rows,
			"pid":           info.PID,
			"working":       info.Working,
			"exited":        info.Exited,
			"claudeEvents":  claudeEvents,
			"lastDataAgeMs": now - info.LastDataAt,
		})
	}
	return result
}

func (r *Registry) runnerEnvironment(info proto.RunnerInfo, caller map[string]string) map[string]string {
	passthroughKeys := []string{
		"SSH_AUTH_SOCK", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy",
		"NODE_EXTRA_CA_CERTS", "GIT_SSH_COMMAND",
		"OPENAI_API_KEY", "SESSIONS_CODEX_APP_SERVER_SOCKET",
	}
	environment := make(map[string]string)
	for _, key := range passthroughKeys {
		if value := os.Getenv(key); value != "" {
			setRunnerEnvironment(environment, key, value)
		}
	}
	addPlatformRunnerEnvironment(environment)
	setRunnerEnvironment(environment, "HOME", getenv("HOME", r.config.DefaultCwd))
	setRunnerEnvironment(environment, "USER", getenv("USER", os.Getenv("USERNAME")))
	setRunnerEnvironment(environment, "PATH", platformRunnerPath(os.Getenv("PATH")))
	setRunnerEnvironment(environment, "LANG", getenv("LANG", "en_US.UTF-8"))
	setRunnerEnvironment(environment, "SHELL", getenv("SHELL", r.config.DefaultShell))
	blocked := map[string]struct{}{
		"NODE_OPTIONS": {}, "DYLD_INSERT_LIBRARIES": {}, "DYLD_LIBRARY_PATH": {}, "LD_PRELOAD": {},
		"CLAUDE_CONFIG_DIR": {}, "CODEX_HOME": {},
	}
	for key, value := range caller {
		normalizedKey := strings.ToUpper(key)
		if strings.HasPrefix(normalizedKey, "RUNNER_") {
			continue
		}
		if _, denied := blocked[normalizedKey]; denied {
			continue
		}
		setRunnerEnvironment(environment, key, value)
	}
	setRunnerEnvironment(environment, "TERM", "xterm-256color")
	// This identity belongs to the newly-created session. Set it after caller
	// environment merging so a caller cannot forge a different descendant.
	setRunnerEnvironment(environment, "SESSIONS_SESSION_ID", info.ID)
	setRunnerEnvironment(environment, "RUNNER_ID", info.ID)
	setRunnerEnvironment(environment, "RUNNER_STATE_DIR", r.config.RunnerStateDir)
	setRunnerEnvironment(environment, "RUNNER_CMD", info.Cmd)
	encodedArgs, _ := json.Marshal(info.Args)
	setRunnerEnvironment(environment, "RUNNER_ARGS_JSON", string(encodedArgs))
	setRunnerEnvironment(environment, "RUNNER_CWD", info.Cwd)
	setRunnerEnvironment(environment, "RUNNER_COLS", fmt.Sprint(info.Cols))
	setRunnerEnvironment(environment, "RUNNER_ROWS", fmt.Sprint(info.Rows))
	return environment
}

func launchdPath(value string) string {
	if value == "" {
		value = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	parts := strings.Split(value, ":")
	contains := func(want string) bool {
		for _, part := range parts {
			if part == want {
				return true
			}
		}
		return false
	}
	home := os.Getenv("HOME")
	candidates := make([]string, 0, 7)
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".npm-global", "bin"),
			filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".cargo", "bin"),
		)
	}
	candidates = append(candidates, "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin")
	prefix := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if !contains(path) {
			prefix = append(prefix, path)
		}
	}
	return strings.Join(append(prefix, parts...), ":")
}

func writeMetadata(dir string, info proto.RunnerInfo, sessionMetadata SessionMetadata) error {
	metadata := Metadata{
		ID: info.ID, Name: sessionMetadata.Name, NameSource: sessionMetadata.NameSource,
		Description:       sessionMetadata.Description,
		DescriptionSource: sessionMetadata.DescriptionSource, Kind: sessionMetadata.Kind, SpecPath: sessionMetadata.SpecPath,
		Tags: CloneTags(sessionMetadata.Tags), DisplayParentSessionID: cloneStringPointer(sessionMetadata.DisplayParentSessionID),
		SetAsideAt: cloneInt64Pointer(sessionMetadata.SetAsideAt), Pinned: sessionMetadata.Pinned,
		LastHumanMessageAt: cloneInt64Pointer(sessionMetadata.LastHumanMessageAt),
		LastAgentMessageAt: cloneInt64Pointer(sessionMetadata.LastAgentMessageAt),
		DelegationKind:     sessionMetadata.DelegationKind,
		Permissions:        sessionMetadata.Permissions, Lifecycle: sessionMetadata.Lifecycle,
		Profile: sessionMetadata.Profile, ConfigDir: sessionMetadata.ConfigDir,
		ContinuedFromHistoryID: sessionMetadata.ContinuedFromHistoryID,
		ContinuedFromProvider:  sessionMetadata.ContinuedFromProvider,
		ContinuationMode:       sessionMetadata.ContinuationMode,
		ImportedMessageCount:   sessionMetadata.ImportedMessageCount,
		Cmd:                    info.Cmd, Args: info.Args, Cwd: info.Cwd,
		Cols: info.Cols, Rows: info.Rows, CreatedAt: info.CreatedAt, PID: info.PID,
		SockPath:       info.SocketPath,
		ConversationID: info.ConversationID, RemoteEndpoint: info.RemoteEndpoint,
		ClaudeSessionID: info.ClaudeSessionID,
	}
	path := filepath.Join(dir, info.ID+".json")
	if err := WriteMetadata(path, metadata); err != nil {
		return fmt.Errorf("write runner metadata: %w", err)
	}
	return nil
}

type RunnerMetadata struct {
	Info                   proto.RunnerInfo
	Name                   string
	NameSource             string
	Description            string
	DescriptionSource      string
	Tags                   map[string]string
	DisplayParentSessionID *string
	SetAsideAt             *int64
	Pinned                 bool
	LastHumanMessageAt     *int64
	LastAgentMessageAt     *int64
	DelegationKind         string
	Permissions            string
	Lifecycle              string
	Kind                   string
	SpecPath               string
	Profile                string
	ConfigDir              string
	ContinuedFromHistoryID string
	ContinuedFromProvider  string
	ContinuationMode       string
	ImportedMessageCount   int
}

func readRunnerMetadata(path string) (RunnerMetadata, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return RunnerMetadata{}, err
	}
	return parseRunnerMetadata(encoded)
}

func parseRunnerMetadata(encoded []byte) (RunnerMetadata, error) {
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return RunnerMetadata{}, err
	}
	if strings.TrimSpace(metadata.ID) == "" {
		return RunnerMetadata{}, errors.New("runner metadata id is required")
	}
	return RunnerMetadata{
		Info: proto.RunnerInfo{
			ID: metadata.ID, Cmd: metadata.Cmd, Args: metadata.Args, Cwd: metadata.Cwd,
			Cols: metadata.Cols, Rows: metadata.Rows, CreatedAt: metadata.CreatedAt,
			PID: metadata.PID, SocketPath: metadata.SockPath,
			ConversationID: metadata.ConversationID, RemoteEndpoint: metadata.RemoteEndpoint,
			ClaudeSessionID: metadata.ClaudeSessionID,
		},
		Name: metadata.Name, NameSource: metadata.NameSource,
		Description: metadata.Description, DescriptionSource: metadata.DescriptionSource,
		Tags: CloneTags(metadata.Tags), DisplayParentSessionID: cloneStringPointer(metadata.DisplayParentSessionID),
		SetAsideAt: cloneInt64Pointer(metadata.SetAsideAt), Pinned: metadata.Pinned,
		LastHumanMessageAt: cloneInt64Pointer(metadata.LastHumanMessageAt),
		LastAgentMessageAt: cloneInt64Pointer(metadata.LastAgentMessageAt),
		DelegationKind:     metadata.DelegationKind,
		Permissions:        metadata.Permissions, Lifecycle: metadata.Lifecycle,
		Kind: metadata.Kind, SpecPath: metadata.SpecPath, Profile: metadata.Profile, ConfigDir: metadata.ConfigDir,
		ContinuedFromHistoryID: metadata.ContinuedFromHistoryID,
		ContinuedFromProvider:  metadata.ContinuedFromProvider,
		ContinuationMode:       metadata.ContinuationMode,
		ImportedMessageCount:   metadata.ImportedMessageCount,
	}, nil
}

// ReadRunnerInfo decodes the canonical runner metadata file for startup
// discovery. It does not mutate or validate filesystem state.
func ReadRunnerInfo(path string) (proto.RunnerInfo, error) {
	metadata, err := readRunnerMetadata(path)
	return metadata.Info, err
}

// ReadRunnerMetadata also returns the optional session label persisted by the
// Go daemon. Older TypeScript runner metadata remains valid because Name is
// optional.
func ReadRunnerMetadata(path string) (RunnerMetadata, error) { return readRunnerMetadata(path) }

func randomUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func classifyTool(cmd string) SessionTool {
	lower := strings.ToLower(cmd)
	switch {
	case lower == "claude" || strings.HasSuffix(lower, "/claude"):
		return ToolClaude
	case lower == "codex" || strings.HasSuffix(lower, "/codex"):
		return ToolCodex
	default:
		return ToolTerminal
	}
}

var toolDefaultArgs = map[string][]string{
	"codex": {"-c", "check_for_update_on_startup=false", "--dangerously-bypass-approvals-and-sandbox"},
}

var explicitModeFlags = map[string]struct{}{
	"--dangerously-bypass-approvals-and-sandbox": {},
	"--dangerously-skip-permissions":             {},
	"--permission-mode":                          {},
	"--sandbox":                                  {}, "-s": {}, "--ask-for-approval": {}, "-a": {}, "--full-auto": {},
}

func withToolDefaultArgs(cmd string, args []string) []string {
	result := append([]string{}, args...)
	defaults := toolDefaultArgs[strings.ToLower(filepath.Base(cmd))]
	if defaults == nil {
		return result
	}
	for _, arg := range result {
		if _, explicit := explicitModeFlags[arg]; explicit {
			return result
		}
	}
	return append(result, defaults...)
}

// appendClaudeSessionID gives a fresh Claude session a stable identity. It must
// recognize every spelling that already names a conversation, including the
// `-r` short form and the joined `--flag=value` form. Missing one made Sessions
// launch `claude -r <uuid> --session-id <fresh uuid>`, which contradicts the
// resume the caller asked for; `sessions new` (cmd/sessions/commands.go) has
// always treated `-r` as a resume flag.
func appendClaudeSessionID(cmd string, args []string, id string) []string {
	result := append([]string{}, args...)
	if classifyTool(cmd) != ToolClaude {
		return result
	}
	if providerargs.HasClaudeIdentity(result) {
		return result
	}
	return append(result, providerargs.ClaudeSessionIDFlag, id)
}

// bindProviderConversationIdentity promotes the provider's own durable
// conversation id from the exact launch argv into RunnerInfo. Runner ids name
// processes; these ids name conversations, so resumed processes with the same
// provider id can be presented as one conversation without rewriting history.
func bindProviderConversationIdentity(info *proto.RunnerInfo, tool SessionTool) {
	if info == nil {
		return
	}
	switch tool {
	case ToolClaude:
		providerID := providerargs.ClaudeSessionID(info.Args)
		if !providerargs.IsConversationUUID(providerID) {
			return
		}
		if info.ConversationID == "" {
			info.ConversationID = providerID
		}
		if info.ClaudeSessionID == "" {
			info.ClaudeSessionID = providerID
		}
	case ToolCodex:
		providerID := providerargs.CodexConversationID(info.Args)
		if info.ConversationID == "" && providerargs.IsConversationUUID(providerID) {
			info.ConversationID = providerID
		}
	}
}

// withProviderIdentityFallback keeps a new runner authoritative when it knows
// its provider identity (structured runners learn it after launch), while
// retaining daemon metadata for PTY and older runners whose HELLO omits it.
func withProviderIdentityFallback(actual, persisted proto.RunnerInfo) proto.RunnerInfo {
	if actual.ConversationID == "" {
		actual.ConversationID = persisted.ConversationID
	}
	if actual.ClaudeSessionID == "" {
		actual.ClaudeSessionID = persisted.ClaudeSessionID
	}
	if actual.RemoteEndpoint == "" {
		actual.RemoteEndpoint = persisted.RemoteEndpoint
	}
	return actual
}

// spawnControls reads the model and effort a spawn argv carries. It used to
// have its own inline copy of both parses, which missed the `--model=opus`
// joined form the CLI accepts.
func spawnControls(tool SessionTool, args []string) (model, effort string, fast bool) {
	model = providerargs.Value(args, providerargs.ModelFlags()...)
	if tool == ToolCodex {
		effort = providerargs.ConfigValue(args, providerargs.CodexEffortKey)
		fast = providerargs.ConfigValue(args, providerargs.CodexServiceTierKey) == "priority"
	} else {
		effort = providerargs.Value(args, providerargs.ClaudeEffortFlags()...)
	}
	return
}
