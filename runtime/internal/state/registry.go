package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	maxClaudeEvents      = 5000
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
		if _, err := writePlist(r.config.LaunchAgentsDir, plistArgs{
			ID:               id,
			ProgramArguments: programArguments,
			Env:              launchRequest.Env,
			Cwd:              cwd,
			LogPath:          filepath.Join(r.config.RunnerStateDir, id+".log"),
		}); err != nil {
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
	if lifecycle.RunnerReady != nil {
		lifecycle.RunnerReady(ctx, actual)
	}
	if err := writeMetadata(r.config.RunnerStateDir, actual, metadata); err != nil {
		return SessionInfo{}, err
	}
	session, err := r.register(ctx, runner, metadata)
	if err != nil {
		return SessionInfo{}, err
	}
	return session.Info(), nil
}

func (r *Registry) register(ctx context.Context, runner proto.Runner, metadata SessionMetadata) (*Session, error) {
	info := runner.Info()
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
	r.mu.Lock()
	if _, exists := r.sessions[info.ID]; exists {
		r.mu.Unlock()
		_ = session.Close()
		return nil, fmt.Errorf("session %s is already registered", info.ID)
	}
	r.sessions[info.ID] = session
	r.order = append(r.order, info.ID)
	r.mu.Unlock()
	session.start(func(event proto.Event) {
		if event.Kind == proto.EventRunnerLost {
			// Match sessions.ts: a lost runner disappears immediately, but its
			// plist and state stay intact so launchd/restart discovery can recover it.
			r.mu.Lock()
			if r.sessions[info.ID] == session {
				delete(r.sessions, info.ID)
				r.removeOrderLocked(info.ID)
			}
			r.mu.Unlock()
			_ = session.Close()
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
	return r.register(ctx, runner, SessionMetadata{
		Name: metadata.Name, NameSource: metadata.NameSource,
		Description: metadata.Description, DescriptionSource: metadata.DescriptionSource,
		Tags: CloneTags(metadata.Tags), DisplayParentSessionID: cloneStringPointer(metadata.DisplayParentSessionID),
		SetAsideAt: cloneInt64Pointer(metadata.SetAsideAt), Pinned: metadata.Pinned,
		LastHumanMessageAt: cloneInt64Pointer(metadata.LastHumanMessageAt),
		LastAgentMessageAt: cloneInt64Pointer(metadata.LastAgentMessageAt),
		DelegationKind: metadata.DelegationKind,
		Permissions:    metadata.Permissions, Lifecycle: metadata.Lifecycle,
		OnIdle: onIdle, Kind: metadata.Kind, SpecPath: metadata.SpecPath,
		Profile: metadata.Profile, ConfigDir: metadata.ConfigDir,
		ContinuedFromHistoryID: metadata.ContinuedFromHistoryID,
		ContinuedFromProvider:  metadata.ContinuedFromProvider,
		ContinuationMode:       metadata.ContinuationMode,
		ImportedMessageCount:   metadata.ImportedMessageCount,
	})
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("read session tags: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return nil, fmt.Errorf("decode session tags: %w", err)
	}
	metadata.Tags = CloneTags(tags)
	if err := WriteMetadata(path, metadata); err != nil {
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("read session name: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return "", fmt.Errorf("decode session name: %w", err)
	}
	metadata.Name = name
	metadata.NameSource = NameSourceExplicit
	if err := WriteMetadata(path, metadata); err != nil {
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return false, fmt.Errorf("read session name: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return false, fmt.Errorf("decode session name: %w", err)
	}
	if metadata.NameSource == NameSourceExplicit {
		if live {
			session.setNameSource(NameSourceExplicit)
		}
		return false, nil
	}
	if metadata.Name == name && metadata.NameSource == NameSourceProvider {
		return false, nil
	}
	metadata.Name = name
	metadata.NameSource = NameSourceProvider
	if err := WriteMetadata(path, metadata); err != nil {
		return false, fmt.Errorf("persist session name: %w", err)
	}
	if live {
		session.setName(name, NameSourceProvider)
	}
	return true, nil
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("read session name: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return "", fmt.Errorf("decode session name: %w", err)
	}
	metadata.NameSource = NameSourceLaunch
	if err := WriteMetadata(path, metadata); err != nil {
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("read session display parent: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return "", fmt.Errorf("decode session display parent: %w", err)
	}
	parent := parentID
	metadata.DisplayParentSessionID = &parent
	if err := WriteMetadata(path, metadata); err != nil {
		return "", fmt.Errorf("persist session display parent: %w", err)
	}
	if live {
		session.setDisplayParentSessionID(parent)
	}
	return parent, nil
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("read session working-set state: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return nil, fmt.Errorf("decode session working-set state: %w", err)
	}
	var setAsideAt *int64
	if setAside {
		now := time.Now().UnixMilli()
		setAsideAt = &now
	}
	metadata.SetAsideAt = cloneInt64Pointer(setAsideAt)
	if err := WriteMetadata(path, metadata); err != nil {
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
//
// This shares the known lost-update window every edit on this document has: a
// concurrent runner write between the read and the write can drop the change.
// The pattern is deliberately not diverged from here.
func (r *Registry) UpdatePinned(id string, pinned bool) (bool, error) {
	if !validMetadataID(id) {
		return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
	}
	session, live := r.Get(id)
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: session %s", ErrSessionNotFound, id)
		}
		return false, fmt.Errorf("read session pin state: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return false, fmt.Errorf("decode session pin state: %w", err)
	}
	metadata.Pinned = pinned
	if err := WriteMetadata(path, metadata); err != nil {
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		return true, err
	}
	var metadata Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return true, err
	}
	if metadata.DescriptionSource == DescriptionExplicit {
		return false, nil
	}
	if !session.setFirstMessageDescription(description) {
		return false, nil
	}
	metadata.Description = description
	metadata.DescriptionSource = DescriptionFirstMessage
	if err := WriteMetadata(path, metadata); err != nil {
		return true, err
	}
	return true, nil
}

// MarkDiscovering exposes startup progress to the API while the higher-level
// session runtime performs its guarded discovery sweep.
func (r *Registry) MarkDiscovering(value bool) { r.setDiscovering(value) }

// SetTerminalObservers lets the session manager record terminal facts while
// Registry remains responsible for the low-level launchd cleanup operation.
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
		_, exists := r.sessions[id]
		r.mu.RUnlock()
		if exists {
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
		Description: sessionMetadata.Description,
		DescriptionSource: sessionMetadata.DescriptionSource, Kind: sessionMetadata.Kind, SpecPath: sessionMetadata.SpecPath,
		Tags: CloneTags(sessionMetadata.Tags), DisplayParentSessionID: cloneStringPointer(sessionMetadata.DisplayParentSessionID),
		SetAsideAt: cloneInt64Pointer(sessionMetadata.SetAsideAt), Pinned: sessionMetadata.Pinned,
		LastHumanMessageAt: cloneInt64Pointer(sessionMetadata.LastHumanMessageAt),
		LastAgentMessageAt: cloneInt64Pointer(sessionMetadata.LastAgentMessageAt),
		DelegationKind: sessionMetadata.DelegationKind,
		Permissions:    sessionMetadata.Permissions, Lifecycle: sessionMetadata.Lifecycle,
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
		DelegationKind: metadata.DelegationKind,
		Permissions:    metadata.Permissions, Lifecycle: metadata.Lifecycle,
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
