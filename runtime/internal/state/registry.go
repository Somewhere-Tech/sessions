package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
)

const (
	defaultEventLogBytes = 4 * 1024 * 1024
	maxClaudeEvents      = proto.MaxStructuredReplayEvents
	exitedGrace          = 30 * time.Second
)

type Registry struct {
	config   Config
	launcher proto.RunnerLauncher
	started  time.Time

	mu           sync.RWMutex
	sessions     map[string]*Session
	order        []string
	discovering  bool
	onRunnerExit func(string, proto.ExitEvent)
	onReaped     func(string)
}

// PreparedSession is the complete, sanitized launch identity exposed to the
// higher-level composition root before any launch side effect occurs.
type PreparedSession struct {
	Info              proto.RunnerInfo
	Name              string
	Description       string
	DescriptionSource string
	Tags              map[string]string
	Kind              string
	SpecPath          string
	Tool              SessionTool
	Profile           string
	ConfigDir         string
	WorktreePath      string
	WorktreeBranch    string
	WorktreeBase      string
	SourceRepo        string
	DelegationKind    string
	Permissions       string
	Lifecycle         string
}

type SessionMetadata struct {
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
	OnIdle                 string
	Kind                   string
	SpecPath               string
	Profile                string
	ConfigDir              string
	ContinuedFromHistoryID string
	ContinuedFromProvider  string
	ContinuationMode       string
	ImportedMessageCount   int
}

// CreateLifecycle lets the session manager place durable boundaries around
// Registry's low-level launch machinery without coupling state to a ledger.
type CreateLifecycle struct {
	BeforeLaunch  func(context.Context, PreparedSession) error
	LaunchStarted func(context.Context, PreparedSession)
	RunnerReady   func(context.Context, proto.RunnerInfo)
}

func NewRegistry(config Config, launcher proto.RunnerLauncher) *Registry {
	return &Registry{
		config:   config,
		launcher: launcher,
		started:  time.Now(),
		sessions: make(map[string]*Session),
	}
}

func (r *Registry) Config() Config        { return r.config }
func (r *Registry) Uptime() time.Duration { return time.Since(r.started) }

func (r *Registry) IsDiscovering() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.discovering
}

func (r *Registry) setDiscovering(value bool) {
	r.mu.Lock()
	r.discovering = value
	r.mu.Unlock()
}

func (r *Registry) Create(ctx context.Context, request CreateSessionRequest) (SessionInfo, error) {
	return r.CreateWithLifecycle(ctx, request, CreateLifecycle{})
}

func (r *Registry) CreateWithLifecycle(
	ctx context.Context,
	request CreateSessionRequest,
	lifecycle CreateLifecycle,
) (SessionInfo, error) {
	if r.launcher == nil {
		return SessionInfo{}, errors.New("runner launcher is unavailable")
	}
	kind := strings.TrimSpace(request.Kind)
	if kind != "" && kind != KindLane && kind != KindCodexAppServer && kind != KindClaudeStructured {
		return SessionInfo{}, fmt.Errorf("unsupported session kind %q", kind)
	}
	cmd := request.Cmd
	if kind == KindLane && strings.TrimSpace(cmd) == "" {
		return SessionInfo{}, errors.New("lane command is required")
	}
	if cmd == "" {
		cmd = r.config.DefaultShell
	}
	cwd := request.Cwd
	if cwd == "" {
		cwd = r.config.DefaultCwd
	}
	info, err := os.Stat(cwd)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionInfo{}, fmt.Errorf("cwd does not exist: %s", cwd)
		}
		return SessionInfo{}, err
	}
	if !info.IsDir() {
		return SessionInfo{}, fmt.Errorf("cwd is not a directory: %s", cwd)
	}

	cols := request.Cols
	if cols == 0 {
		cols = r.config.DefaultCols
	}
	rows := request.Rows
	if rows == 0 {
		rows = r.config.DefaultRows
	}
	id, err := randomUUID()
	if err != nil {
		return SessionInfo{}, fmt.Errorf("generate session id: %w", err)
	}
	args := append([]string{}, request.Args...)
	tool := classifyTool(cmd)
	if kind == KindLane {
		tool = ToolLane
	} else if kind == KindCodexAppServer {
		if tool != ToolCodex {
			return SessionInfo{}, errors.New("codex app-server sessions require the codex command")
		}
	} else if kind == KindClaudeStructured {
		if tool != ToolClaude {
			return SessionInfo{}, errors.New("structured Claude sessions require the claude command")
		}
	} else {
		args = appendClaudeSessionID(cmd, args, id)
		args = withToolDefaultArgs(cmd, args)
	}
	profile := request.Profile
	configDir := request.ConfigDir
	if profile != "" {
		if err := ValidateProfileName(profile); err != nil {
			return SessionInfo{}, err
		}
		if _, supported := ProfileToolName(tool); !supported {
			return SessionInfo{}, errors.New("--profile is only for Claude or Codex sessions; remove it for shell sessions")
		}
		if configDir == "" || !filepath.IsAbs(configDir) {
			return SessionInfo{}, errors.New("profile config directory must be an absolute path")
		}
	} else if configDir != "" {
		return SessionInfo{}, errors.New("profile config directory requires a profile name")
	}
	specPath := strings.TrimSpace(request.SpecPath)
	if specPath != "" {
		if !filepath.IsAbs(specPath) {
			specPath = filepath.Join(cwd, specPath)
		}
		specPath, err = filepath.Abs(specPath)
		if err != nil {
			return SessionInfo{}, fmt.Errorf("resolve spec path: %w", err)
		}
	}

	createdAt := time.Now().UnixMilli()
	runnerInfo := proto.RunnerInfo{
		ID:             id,
		Cmd:            cmd,
		Args:           args,
		Cwd:            cwd,
		Cols:           cols,
		Rows:           rows,
		CreatedAt:      createdAt,
		SocketPath:     For(r.config.RunnerStateDir, id).Socket,
		ConversationID: request.ConversationID,
	}
	bindProviderConversationIdentity(&runnerInfo, tool)
	launchRequest := proto.LaunchRequest{
		Info: runnerInfo,
		Env:  r.runnerEnvironment(runnerInfo, request.Env),
	}
	if profile != "" {
		envKey := "CODEX_HOME"
		if tool == ToolClaude {
			envKey = "CLAUDE_CONFIG_DIR"
		}
		launchRequest.Env[envKey] = configDir
		launchRequest.Env["RUNNER_PROFILE"] = profile
		launchRequest.Env["RUNNER_CONFIG_DIR"] = configDir
	}
	if kind == KindClaudeStructured {
		// Structured Claude is intentionally subscription-authenticated. Never
		// place a metered API key in its launchd environment.
		delete(launchRequest.Env, "ANTHROPIC_API_KEY")
	}
	description := strings.TrimSpace(request.Description)
	tags, err := NormalizeTags(request.Tags)
	if err != nil {
		return SessionInfo{}, err
	}
	descriptionSource := ""
	if description != "" {
		descriptionSource = DescriptionExplicit
	}
	prepared := PreparedSession{
		Info: runnerInfo, Name: strings.TrimSpace(request.Name), Description: description,
		DescriptionSource: descriptionSource, Tags: tags, Kind: kind, SpecPath: specPath, Tool: tool,
		Profile: profile, ConfigDir: configDir,
		WorktreePath: request.WorktreePath, WorktreeBranch: request.WorktreeBranch,
		WorktreeBase: request.WorktreeBase, SourceRepo: request.SourceRepo,
		DelegationKind: request.DelegationKind, Permissions: request.Permissions, Lifecycle: request.Lifecycle,
	}
	if prepared.Name != "" {
		launchRequest.Env["RUNNER_NAME"] = prepared.Name
	}
	if prepared.Description != "" {
		launchRequest.Env["RUNNER_DESCRIPTION"] = prepared.Description
		launchRequest.Env["RUNNER_DESCRIPTION_SOURCE"] = prepared.DescriptionSource
	}
	if len(prepared.Tags) > 0 {
		encodedTags, _ := json.Marshal(prepared.Tags)
		launchRequest.Env["RUNNER_TAGS_JSON"] = string(encodedTags)
	}
	if prepared.Kind != "" {
		launchRequest.Env["RUNNER_KIND"] = prepared.Kind
	}
	if prepared.SpecPath != "" {
		launchRequest.Env["RUNNER_SPEC_PATH"] = prepared.SpecPath
	}
	if request.Continuation != nil {
		if _, err := continuationBytes(*request.Continuation); err != nil {
			return SessionInfo{}, fmt.Errorf("validate continuation: %w", err)
		}
	}
	programArguments := r.launcher.ProgramArguments(launchRequest)
	if len(programArguments) == 0 || !isExecutableFile(programArguments[0]) {
		return SessionInfo{}, errors.New("runner executable unavailable: set SESSIONS_RUNNER to an absolute path to an existing executable")
	}
	if preflight, ok := r.launcher.(proto.RunnerLaunchPreflight); ok {
		if err := preflight.Preflight(launchRequest); err != nil {
			return SessionInfo{}, err
		}
	}
	if err := os.MkdirAll(r.config.RunnerStateDir, 0o700); err != nil {
		return SessionInfo{}, fmt.Errorf("create runner state directory: %w", err)
	}
	if lifecycle.BeforeLaunch != nil {
		if err := lifecycle.BeforeLaunch(ctx, prepared); err != nil {
			return SessionInfo{}, err
		}
	}
	metadata := SessionMetadata{
		Name: prepared.Name, Description: prepared.Description, DescriptionSource: prepared.DescriptionSource,
		Tags: CloneTags(prepared.Tags), DisplayParentSessionID: cloneStringPointer(request.DisplayParentSessionID),
		OnIdle: strings.TrimSpace(request.OnIdle), Kind: prepared.Kind, SpecPath: prepared.SpecPath,
		Profile: prepared.Profile, ConfigDir: prepared.ConfigDir, DelegationKind: prepared.DelegationKind,
		Permissions: prepared.Permissions, Lifecycle: prepared.Lifecycle,
	}
	if request.Continuation != nil {
		continuation := *request.Continuation
		if err := continuation.Validate(); err != nil {
			return SessionInfo{}, fmt.Errorf("validate continuation: %w", err)
		}
		metadata.ContinuedFromHistoryID = continuation.SourceHistoryID
		metadata.ContinuedFromProvider = continuation.SourceProvider
		metadata.ContinuationMode = continuation.Mode
		metadata.ImportedMessageCount = len(continuation.Messages)
		if err := WriteContinuation(For(r.config.RunnerStateDir, runnerInfo.ID).Continuation, continuation); err != nil {
			return SessionInfo{}, fmt.Errorf("write continuation sidecar: %w", err)
		}
	}
	if err := writeMetadata(r.config.RunnerStateDir, runnerInfo, metadata); err != nil {
		return SessionInfo{}, err
	}
	if preparer, ok := r.launcher.(interface {
		Prepare(proto.LaunchRequest) error
	}); ok {
		if err := preparer.Prepare(launchRequest); err != nil {
			return SessionInfo{}, err
		}
	} else if runtime.GOOS != "windows" {
		// Keep custom Unix launchers and tests compatible with the original
		// registry contract. Windows launchers deliberately have no plist.
		paths := For(r.config.RunnerStateDir, id)
		bootID, bootErr := CurrentBootID()
		if bootErr != nil {
			return SessionInfo{}, fmt.Errorf("prepare runner restart policy: %w", bootErr)
		}
		if err := WriteRestartPermit(paths.KeepAlive, bootID); err != nil {
			return SessionInfo{}, fmt.Errorf("prepare runner restart permit: %w", err)
		}
		launchRequest.Env["RUNNER_RESTART_POLICY"] = "boot-scoped"
		if _, err := writePlist(r.config.LaunchAgentsDir, plistArgs{
			ID:               id,
			ProgramArguments: programArguments,
			Env:              launchRequest.Env,
			Cwd:              cwd,
			LogPath:          filepath.Join(r.config.RunnerStateDir, id+".log"),
			KeepAlivePath:    paths.KeepAlive,
		}); err != nil {
			_ = os.Remove(paths.KeepAlive)
			return SessionInfo{}, err
		}
	}
	if lifecycle.LaunchStarted != nil {
		lifecycle.LaunchStarted(ctx, prepared)
	}

	runner, err := r.launcher.Launch(ctx, launchRequest)
	if err != nil {
		// A failed create has no registered session that can own a lingering
		// service. Remove only this exact launch registration so a runner that
		// never published its socket cannot keep spinning invisibly. Metadata
		// and logs remain available for diagnosis and recovery.
		if reaper, ok := r.launcher.(interface{ Reap(string) error }); ok {
			if reapErr := reaper.Reap(id); reapErr != nil {
				return SessionInfo{}, errors.Join(err, fmt.Errorf("stop failed runner %s: %w", id, reapErr))
			}
		}
		return SessionInfo{}, err
	}
	actual := runner.Info()
	if actual.ID != id {
		return SessionInfo{}, fmt.Errorf("runner id mismatch: got %q, want %q", actual.ID, id)
	}
	if actual.SocketPath == "" {
		actual.SocketPath = runnerInfo.SocketPath
	}
	actual = withProviderIdentityFallback(actual, runnerInfo)
	if lifecycle.RunnerReady != nil {
		lifecycle.RunnerReady(ctx, actual)
	}
	if err := writeMetadata(r.config.RunnerStateDir, actual, metadata); err != nil {
		return SessionInfo{}, err
	}
	session, err := r.registerInfo(ctx, runner, metadata, actual)
	if err != nil {
		return SessionInfo{}, err
	}
	return session.Info(), nil
}

func (r *Registry) register(ctx context.Context, runner proto.Runner, metadata SessionMetadata) (*Session, error) {
	return r.registerInfo(ctx, runner, metadata, runner.Info())
}

func (r *Registry) registerInfo(ctx context.Context, runner proto.Runner, metadata SessionMetadata, info proto.RunnerInfo) (*Session, error) {
	if info.ID == "" {
		return nil, errors.New("runner returned an empty session id")
	}
	if !proto.IsCompatibleVersion(info.ProtocolVersion) {
		return nil, proto.IncompatibleVersionError(info.ProtocolVersion)
	}
	if info.ProtocolVersion != proto.ProtocolVersion {
		log.Printf(
			"[protocol] runner %s reports compatible v%d, daemon current is v%d",
			info.ID, info.ProtocolVersion, proto.ProtocolVersion,
		)
	}
	session, err := newSession(ctx, info, runner, metadata)
	if err != nil {
		return nil, err
	}
	var replaced *Session
	r.mu.Lock()
	if existing, exists := r.sessions[info.ID]; exists {
		// A socket loss is not a lifecycle end. Keep that durable session in
		// the registry so reads and controls do not turn into a transient 404,
		// then atomically replace only that unreachable connection when the
		// same runner comes back. A reachable duplicate remains an ownership
		// error: two live connections must never race to control one runner.
		if !existing.Info().Unreachable {
			r.mu.Unlock()
			_ = session.Close()
			return nil, fmt.Errorf("session %s is already registered", info.ID)
		}
		replaced = existing
		r.sessions[info.ID] = session
	} else {
		r.sessions[info.ID] = session
		r.order = append(r.order, info.ID)
	}
	r.mu.Unlock()
	if replaced != nil {
		_ = replaced.Close()
	}
	session.start(func(event proto.Event) {
		if event.Kind == proto.EventRunnerLost {
			// The Session already recorded Unreachable before this callback.
			// Retain it as the readable/control-plane placeholder until a
			// successful reconnect replaces this exact object above.
			return
		}
		if r.onRunnerExit != nil {
			r.onRunnerExit(info.ID, event.Exit)
		}
		reaped := false
		if reaper, ok := r.launcher.(interface{ Reap(string) error }); ok {
			reaped = reaper.Reap(info.ID) == nil
		} else {
			err := os.Remove(plistPath(r.config.LaunchAgentsDir, info.ID))
			reaped = err == nil || errors.Is(err, os.ErrNotExist)
		}
		if reaped && r.onReaped != nil {
			r.onReaped(info.ID)
		}
		time.AfterFunc(exitedGrace, func() {
			r.mu.Lock()
			if r.sessions[info.ID] == session {
				delete(r.sessions, info.ID)
				r.removeOrderLocked(info.ID)
			}
			r.mu.Unlock()
			_ = session.Close()
		})
	})
	return session, nil
}

// Register attaches an already-probed runner to the in-memory registry. The
// session runtime uses it after applying the conservative discovery policy.
func (r *Registry) Register(ctx context.Context, runner proto.Runner, name, onIdle string) (*Session, error) {
	return r.register(ctx, runner, SessionMetadata{Name: name, OnIdle: onIdle})
}

func (r *Registry) RegisterMetadata(ctx context.Context, runner proto.Runner, metadata RunnerMetadata, onIdle string) (*Session, error) {
	actual := withProviderIdentityFallback(runner.Info(), metadata.Info)
	return r.registerInfo(ctx, runner, SessionMetadata{
		Name: metadata.Name, NameSource: metadata.NameSource,
		Description: metadata.Description, DescriptionSource: metadata.DescriptionSource,
		Tags: CloneTags(metadata.Tags), DisplayParentSessionID: cloneStringPointer(metadata.DisplayParentSessionID),
		SetAsideAt: cloneInt64Pointer(metadata.SetAsideAt), Pinned: metadata.Pinned,
		LastHumanMessageAt: cloneInt64Pointer(metadata.LastHumanMessageAt),
		LastAgentMessageAt: cloneInt64Pointer(metadata.LastAgentMessageAt),
		DelegationKind:     metadata.DelegationKind,
		Permissions:        metadata.Permissions, Lifecycle: metadata.Lifecycle,
		OnIdle: onIdle, Kind: metadata.Kind, SpecPath: metadata.SpecPath,
		Profile: metadata.Profile, ConfigDir: metadata.ConfigDir,
		ContinuedFromHistoryID: metadata.ContinuedFromHistoryID,
		ContinuedFromProvider:  metadata.ContinuedFromProvider,
		ContinuationMode:       metadata.ContinuationMode,
		ImportedMessageCount:   metadata.ImportedMessageCount,
	}, actual)
}

// UpdateTags replaces one session's complete tag set. The metadata file is
// written before the in-memory view changes so an acknowledged edit always
// survives daemon restart and runner re-adoption.
func (r *Registry) UpdateTags(id string, requested map[string]string) (map[string]string, error) {
	if !validMetadataID(id) {
		return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	tags, err := NormalizeTags(requested)
	if err != nil {
		return nil, err
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	_, err = updateMetadata(path, func(metadata *Metadata) error {
		metadata.Tags = CloneTags(tags)
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("persist session tags: %w", err)
	}
	if live {
		session.setTags(tags)
	}
	return CloneTags(tags), nil
}

// validateSessionName is the single rule for what may become a session's
// stored name, whether a person typed it or the daemon adopted it from the
// provider's own conversation title.
func validateSessionName(requested string) (string, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return "", errors.New("session name is required")
	}
	if len([]rune(name)) > 120 {
		return "", errors.New("session name must be 120 characters or fewer")
	}
	if strings.IndexFunc(name, func(value rune) bool {
		return value < ' ' || value == '\u007f'
	}) >= 0 {
		return "", errors.New("session name cannot contain control characters")
	}
	return name, nil
}

// UpdateName persists the canonical Sessions title as the user's own choice.
// Recording NameSourceExplicit is what stops the daemon from following the
// provider's conversation title afterwards; from here the card is the user's
// until `sessions rename --auto` releases it. Sessions never rewrites a
// provider's private conversation files to imitate an unsupported rename API.
func (r *Registry) UpdateName(id, requested string) (string, error) {
	if !validMetadataID(id) {
		return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	name, err := validateSessionName(requested)
	if err != nil {
		return "", err
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	_, err = updateMetadata(path, func(metadata *Metadata) error {
		metadata.Name = name
		metadata.NameSource = NameSourceExplicit
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("persist session name: %w", err)
	}
	if live {
		session.setName(name, NameSourceExplicit)
	}
	return name, nil
}

// AdoptProviderTitle follows the provider's own conversation title.
//
// Claude titles conversations, and that title is what every Claude surface
// shows. A Sessions card that keeps its launch-time auto-name disagrees with
// the provider about the same conversation forever: the user searches Sessions
// for the name they can see in Claude and finds nothing. The daemon therefore
// adopts the provider's title, and keeps adopting later changes to it, which
// is the only way the two stay in agreement.
//
// This is the daemon speaking, not the user, so it never overrides a name a
// person chose. NameSourceExplicit is checked in memory and again on disk: the
// on-disk document is the one that survives a restart, and a session being
// adopted may not be live at all. A title that is not a legal session name --
// empty, whitespace-only, control characters -- is not adopted and is not an
// error, because there is nothing a caller could do about a provider record.
func (r *Registry) AdoptProviderTitle(id, title string) (bool, error) {
	if !validMetadataID(id) {
		return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	name, err := validateSessionName(CompactConversationTitle(title))
	if err != nil {
		return false, nil
	}
	session, live := r.Get(id)
	if live && session.Info().NameSource == NameSourceExplicit {
		return false, nil
	}
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	changed := false
	explicit := false
	_, err = updateMetadata(path, func(metadata *Metadata) error {
		if metadata.NameSource == NameSourceExplicit {
			explicit = true
			return nil
		}
		if metadata.Name == name && metadata.NameSource == NameSourceProvider {
			return nil
		}
		metadata.Name = name
		metadata.NameSource = NameSourceProvider
		changed = true
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return false, fmt.Errorf("persist session name: %w", err)
	}
	if explicit {
		if live {
			session.setNameSource(NameSourceExplicit)
		}
		return false, nil
	}
	if !changed {
		return false, nil
	}
	if live {
		session.setName(name, NameSourceProvider)
	}
	return true, nil
}

// BindProviderConversation persists an identity learned from the provider's
// own transcript. Fresh terminal-backed Codex sessions do not know their
// conversation UUID at process launch; the watcher learns it only after an
// exact submitted-message match selects one rollout. Keeping that binding only
// in memory made every daemon restart forget the assistant side of the
// conversation until another message happened to be sent.
//
// An existing, different identity is never replaced here. Changing ownership
// is a recovery operation with its own ledger boundary, not a side effect of a
// transcript watcher.
func (r *Registry) BindProviderConversation(id, conversationID string) (bool, error) {
	if !validMetadataID(id) {
		return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	conversationID = strings.TrimSpace(conversationID)
	if !providerargs.IsConversationUUID(conversationID) {
		return false, errors.New("provider conversation id must be a canonical UUID")
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	changed := false
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		existing := strings.TrimSpace(metadata.ConversationID)
		if existing != "" && existing != conversationID {
			return fmt.Errorf("session is already bound to provider conversation %s", existing)
		}
		if existing == conversationID {
			return nil
		}
		metadata.ConversationID = conversationID
		changed = true
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return false, fmt.Errorf("persist provider conversation: %w", err)
	}
	if changed && live {
		session.setConversationID(conversationID)
	}
	return changed, nil
}

// ReleaseName hands the card back to the provider after an explicit rename.
// The stored name changes only if the provider has already titled the
// conversation; otherwise the current name stays and the next title adopts.
func (r *Registry) ReleaseName(id string) (string, error) {
	if !validMetadataID(id) {
		return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	metadata, err := updateMetadata(path, func(metadata *Metadata) error {
		metadata.NameSource = NameSourceLaunch
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("persist session name: %w", err)
	}
	if !live {
		return metadata.Name, nil
	}
	session.setNameSource(NameSourceLaunch)
	info := session.Info()
	title := ProviderConversationTitle(info.ClaudeCustomTitle, info.ClaudeAITitle)
	if title == "" {
		return info.Name, nil
	}
	if _, err := r.AdoptProviderTitle(id, title); err != nil {
		return "", err
	}
	return session.Info().Name, nil
}

// UpdateDisplayParent persists only the user's visual organization. Trusted
// creator provenance remains in the append-only ledger and is intentionally
// not modified by drag-and-drop in the app.
func (r *Registry) UpdateDisplayParent(id, parentID string) (string, error) {
	if !validMetadataID(id) {
		return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		parent := parentID
		metadata.DisplayParentSessionID = &parent
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("persist session display parent: %w", err)
	}
	if live {
		session.setDisplayParentSessionID(parentID)
	}
	return parentID, nil
}

// UpdateSetAside persists working-set organization without changing runner
// lifecycle. Clearing stores nil so older runtimes continue to decode the
// metadata and the absence remains the default.
func (r *Registry) UpdateSetAside(id string, setAside bool) (*int64, error) {
	if !validMetadataID(id) {
		return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	var setAsideAt *int64
	if setAside {
		now := time.Now().UnixMilli()
		setAsideAt = &now
	}
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		metadata.SetAsideAt = cloneInt64Pointer(setAsideAt)
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("persist session working-set state: %w", err)
	}
	if live {
		session.setSetAsideAt(setAsideAt)
	}
	return cloneInt64Pointer(setAsideAt), nil
}

// UpdatePinned persists the user's decision to keep a session as a workbench.
// The metadata file is written before the in-memory view changes, exactly as
// the neighbouring edits do, so an acknowledged pin survives daemon restart and
// runner re-adoption; that durability is most of what a pin is for, because the
// automatic machinery it exempts a session from runs after a restart too.
func (r *Registry) UpdatePinned(id string, pinned bool) (bool, error) {
	if !validMetadataID(id) {
		return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		metadata.Pinned = pinned
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return false, fmt.Errorf("persist session pin state: %w", err)
	}
	if live {
		session.setPinned(pinned)
	}
	return pinned, nil
}

func (r *Registry) Tags(id string) (map[string]string, error) {
	if !validMetadataID(id) {
		return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	if session, ok := r.Get(id); ok {
		return CloneTags(session.Info().Tags), nil
	}
	metadata, err := readRunnerMetadata(filepath.Join(r.config.RunnerStateDir, id+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("read session tags: %w", err)
	}
	return CloneTags(metadata.Tags), nil
}

func validMetadataID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\\`)
}

// SetFirstMessageDescription records a best-effort purpose without replacing
// an explicit description supplied at creation time.
func (r *Registry) SetFirstMessageDescription(id, description string) (bool, error) {
	session, ok := r.Get(id)
	if !ok || session.Info().DescriptionSource == DescriptionExplicit {
		return false, nil
	}
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	blocked := false
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		if metadata.DescriptionSource == DescriptionExplicit {
			blocked = true
			return nil
		}
		metadata.Description = description
		metadata.DescriptionSource = DescriptionFirstMessage
		return nil
	})
	if err != nil {
		return true, err
	}
	if blocked {
		return false, nil
	}
	if !session.setFirstMessageDescription(description) {
		return false, nil
	}
	return true, nil
}

// MarkDiscovering exposes startup progress to the API while the higher-level
// session runtime performs its guarded discovery sweep.
func (r *Registry) MarkDiscovering(value bool) { r.setDiscovering(value) }

// SetTerminalObservers lets the session manager record terminal facts while
// Registry remains responsible for the low-level launchd cleanup operation.
