// Package recovery reconciles Sessions' durable lane ledger with observable
// runner and provider state. Observation is deliberately read-only: reports
// never attach a runner to the daemon, alter launchd, or adopt conversations.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const probeTimeout = 350 * time.Millisecond

// EventReader is the read-only ledger surface used during reconciliation.
type EventReader interface {
	Events(context.Context, string) ([]ledger.Event, error)
}

type LaunchdStatus struct {
	Loaded  bool `json:"loaded"`
	Running bool `json:"running"`
}

type LaunchdProbe func(context.Context, string) (LaunchdStatus, error)
type HelloProbe func(context.Context, string) (proto.RunnerInfo, error)

// ProcessProbe reports whether the process recorded in a runner's metadata is
// still running and is still that runner. It is the platform-neutral liveness
// signal: launchd answers only on darwin, so without this Windows and Linux
// lose one of recovery's three independent ways to see that a session is alive.
type ProcessProbe func(context.Context, proto.RunnerInfo) bool

type Options struct {
	Reader            EventReader
	RunnerStateDir    string
	ClaudeProjectsDir string
	CodexSessionsDir  string
	ManagedSessions   []state.SessionInfo
	LaunchdProbe      LaunchdProbe
	HelloProbe        HelloProbe
	ProcessProbe      ProcessProbe
	Clock             func() time.Time
}

type Engine struct{ options Options }

func New(options Options) *Engine {
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.LaunchdProbe == nil {
		options.LaunchdProbe = probeLaunchd
	}
	if options.HelloProbe == nil {
		options.HelloProbe = probeHello
	}
	if options.ProcessProbe == nil {
		options.ProcessProbe = probeProcess
	}
	return &Engine{options: options}
}

// Reality is what could be observed about one lane, signal by signal. Nothing
// here is a conclusion; classification combines them.
type Reality struct {
	MetadataPresent bool `json:"metadataPresent"`
	SocketPresent   bool `json:"socketPresent"`
	Hello           bool `json:"hello"`
	// LaunchdLoaded and LaunchdRunning are darwin-only by construction: the
	// probe reports false everywhere else. ProcessAlive is the signal that
	// answers on every platform.
	LaunchdLoaded  bool `json:"launchdLoaded"`
	LaunchdRunning bool `json:"launchdRunning"`
	ProcessAlive   bool `json:"processAlive"`
	ManagerVisible bool `json:"managerVisible"`
	// Conversation is the provider's own conversation file. Its presence is
	// what makes a native `claude --resume` or `codex resume` possible.
	Conversation string `json:"conversation,omitempty"`
	// TranscriptMirror is Sessions' own copy of the conversation, when one
	// exists and holds records. It is deliberately a separate fact from
	// Conversation: a mirror makes the conversation readable and recoverable
	// through Sessions, and never makes a provider-native resume work.
	TranscriptMirror string `json:"transcriptMirror,omitempty"`
	// ConversationRecoverable reports whether the conversation can still be
	// read from somewhere — the provider's file or Sessions' mirror. It is not
	// a promise that native resume works; that remains Conversation.
	ConversationRecoverable bool     `json:"conversationRecoverable"`
	ProbeErrors             []string `json:"probeErrors,omitempty"`
}

type Lane struct {
	ID                       string                `json:"id"`
	Name                     string                `json:"name,omitempty"`
	Tool                     string                `json:"tool,omitempty"`
	Cwd                      string                `json:"cwd,omitempty"`
	Profile                  string                `json:"profile,omitempty"`
	ConfigDir                string                `json:"config_dir,omitempty"`
	ProviderUUID             string                `json:"providerUuid,omitempty"`
	Class                    ledger.Class          `json:"class"`
	Anomalies                []ledger.Anomaly      `json:"anomalies"`
	CreatedAtMS              int64                 `json:"createdAtMs,omitempty"`
	LastEventAtMS            int64                 `json:"lastEventAtMs,omitempty"`
	LastActivityAtMS         int64                 `json:"lastActivityAtMs,omitempty"`
	LastHumanInputAtMS       int64                 `json:"lastHumanInputAtMs,omitempty"`
	LastProviderActivityAtMS int64                 `json:"lastProviderActivityAtMs,omitempty"`
	LastActivitySource       ledger.ActivitySource `json:"lastActivitySource,omitempty"`
	ReopenedAs               string                `json:"reopenedAs,omitempty"`
	ResumeArgv               []string              `json:"resumeArgv,omitempty"`
	Reality                  Reality               `json:"reality"`
}

type Report struct {
	GeneratedAtMS int64               `json:"generatedAtMs"`
	Lanes         []Lane              `json:"lanes"`
	Plan          ledger.RecoveryPlan `json:"plan"`
}

type observedMetadata struct {
	raw proto.RunnerInfo
}

func (e *Engine) Report(ctx context.Context) (Report, error) {
	if e.options.Reader == nil {
		return Report{}, errors.New("recovery ledger reader is required")
	}
	events, err := e.options.Reader.Events(ctx, "")
	if err != nil {
		return Report{}, fmt.Errorf("read recovery ledger: %w", err)
	}
	states := ledger.Fold(events)
	stateByID := make(map[string]ledger.LaneState, len(states))
	candidates := make(map[string]struct{}, len(states))
	for _, lane := range states {
		stateByID[lane.LaneID] = lane
		candidates[lane.LaneID] = struct{}{}
	}

	metadata, metadataErrors := e.readMetadata()
	for id := range metadata {
		candidates[id] = struct{}{}
	}
	for id := range metadataErrors {
		candidates[id] = struct{}{}
	}
	managed := make(map[string]state.SessionInfo, len(e.options.ManagedSessions))
	for _, info := range e.options.ManagedSessions {
		if info.ID == "" || info.Exited {
			continue
		}
		managed[info.ID] = info
		candidates[info.ID] = struct{}{}
	}

	runtimeStates := make(map[string]ledger.RuntimeState, len(candidates))
	realities := make(map[string]Reality, len(candidates))
	for id := range candidates {
		reality := Reality{}
		if message := metadataErrors[id]; message != "" {
			reality.ProbeErrors = append(reality.ProbeErrors, message)
		}
		observed, hasMetadata := metadata[id]
		reality.MetadataPresent = hasMetadata
		reality.ManagerVisible = managed[id].ID != ""

		socketPath := state.For(e.options.RunnerStateDir, id).Socket
		if hasMetadata && observed.raw.SocketPath != "" {
			socketPath = observed.raw.SocketPath
		}
		if ipc.MayExist(socketPath) {
			reality.SocketPresent = ipc.HasFilesystemMarker()
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			hello, helloErr := e.options.HelloProbe(probeCtx, socketPath)
			cancel()
			if helloErr != nil {
				reality.ProbeErrors = append(reality.ProbeErrors, "hello: "+helloErr.Error())
			} else if hello.ID != id {
				reality.ProbeErrors = append(reality.ProbeErrors, fmt.Sprintf("hello: runner id %q does not match metadata id", hello.ID))
			} else {
				reality.SocketPresent = true
				reality.Hello = true
				if !hasMetadata {
					observed = observedFromRunner(hello)
					metadata[id] = observed
				}
			}
		}

		launchdCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		launchd, launchdErr := e.options.LaunchdProbe(launchdCtx, id)
		cancel()
		if launchdErr != nil {
			reality.ProbeErrors = append(reality.ProbeErrors, "launchd: "+launchdErr.Error())
		}
		reality.LaunchdLoaded = launchd.Loaded
		reality.LaunchdRunning = launchd.Running

		// launchd answers only on darwin. Without this probe a Windows or Linux
		// lane whose socket has gone quiet has no third opinion at all, and a
		// live session reads as unexpectedly lost.
		// observed is whatever identity was established above, from the metadata
		// file or from HELLO itself, so a runner with no metadata file still
		// gets its pid probed.
		if observed.raw.PID > 0 {
			processCtx, cancelProcess := context.WithTimeout(ctx, probeTimeout)
			reality.ProcessAlive = e.options.ProcessProbe(processCtx, observed.raw)
			cancelProcess()
		}

		lane := stateByID[id]
		known, exists, conversation := e.resumeSource(lane)
		reality.Conversation = conversation
		// The mirror is Sessions' own copy. It is reported beside the provider
		// file and never folded into ResumeSourceExists, which load-bears
		// "native provider resume will work": a mirror cannot make
		// `claude --resume` succeed, and promising that would produce a resume
		// that fails in the user's terminal.
		mirrorPath := watch.TranscriptMirrorPath(e.options.RunnerStateDir, id)
		if watch.TranscriptMirrorUsable(mirrorPath) {
			reality.TranscriptMirror = mirrorPath
		}
		reality.ConversationRecoverable = exists || reality.TranscriptMirror != ""
		runtimeStates[id] = ledger.RuntimeState{
			Running: reality.ManagerVisible || reality.Hello ||
				reality.LaunchdRunning || reality.ProcessAlive,
			ResumeSourceKnown:  known,
			ResumeSourceExists: exists,
			// Carried separately from ResumeSourceExists so classification can
			// keep raising the missing-source anomaly -- the provider file
			// really is gone -- while the plan stops calling the conversation
			// unrecoverable.
			TranscriptMirrorUsable: reality.TranscriptMirror != "",
		}
		realities[id] = reality
	}

	// Runtime-only lanes carry enough metadata to make external rows useful,
	// while Created remains false so the ledger still owns classification.
	for id, runtimeState := range runtimeStates {
		if _, exists := stateByID[id]; exists || !runtimeState.Running {
			continue
		}
		if observed, ok := metadata[id]; ok {
			stateByID[id] = externalLane(id, observed.raw)
		} else if info, ok := managed[id]; ok {
			stateByID[id] = externalSessionLane(info)
		} else {
			stateByID[id] = ledger.LaneState{LaneID: id}
		}
	}
	allStates := make([]ledger.LaneState, 0, len(stateByID))
	for _, lane := range stateByID {
		allStates = append(allStates, lane)
	}
	classified := ledger.ClassifyAll(allStates, runtimeStates)
	plan := ledger.BuildRecoveryPlan(classified)

	lanes := make([]Lane, 0, len(classified))
	for _, classification := range classified {
		lane := classification.Lane
		lanes = append(lanes, Lane{
			ID: lane.LaneID, Name: lane.Name, Tool: lane.Tool, Cwd: lane.Cwd,
			Profile: lane.Profile, ConfigDir: lane.ConfigDir,
			ProviderUUID: lane.ProviderUUID, Class: classification.Class,
			Anomalies:   append([]ledger.Anomaly{}, classification.Anomalies...),
			CreatedAtMS: lane.CreatedAtMS, LastEventAtMS: lane.LastEventAtMS,
			LastActivityAtMS:         lane.LastActivityAtMS,
			LastHumanInputAtMS:       lane.LastHumanInputAtMS,
			LastProviderActivityAtMS: lane.LastProviderActivityAtMS,
			LastActivitySource:       lane.LastActivitySource, ReopenedAs: lane.ReopenedAs,
			ResumeArgv: append([]string(nil), lane.ResumeArgv...), Reality: realities[lane.LaneID],
		})
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		if lanes[i].LastActivityAtMS != lanes[j].LastActivityAtMS {
			return lanes[i].LastActivityAtMS > lanes[j].LastActivityAtMS
		}
		if lanes[i].LastActivitySource != lanes[j].LastActivitySource {
			return lanes[i].LastActivitySource == ledger.ActivityHumanInput
		}
		if lanes[i].Name != lanes[j].Name {
			return lanes[i].Name < lanes[j].Name
		}
		return lanes[i].ID < lanes[j].ID
	})
	return Report{GeneratedAtMS: e.options.Clock().UnixMilli(), Lanes: lanes, Plan: plan}, nil
}

func (e *Engine) readMetadata() (map[string]observedMetadata, map[string]string) {
	result := make(map[string]observedMetadata)
	failures := make(map[string]string)
	entries, err := os.ReadDir(e.options.RunnerStateDir)
	if errors.Is(err, os.ErrNotExist) || e.options.RunnerStateDir == "" {
		return result, failures
	}
	if err != nil {
		failures["state-dir"] = "metadata: " + err.Error()
		return result, failures
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		info, readErr := state.ReadRunnerInfo(filepath.Join(e.options.RunnerStateDir, entry.Name()))
		if readErr != nil {
			failures[id] = "metadata: " + readErr.Error()
			continue
		}
		if info.ID != id {
			failures[id] = fmt.Sprintf("metadata: id is %q", info.ID)
			continue
		}
		result[id] = observedFromRunner(info)
	}
	return result, failures
}

func observedFromRunner(info proto.RunnerInfo) observedMetadata {
	return observedMetadata{raw: info}
}

func externalLane(id string, info proto.RunnerInfo) ledger.LaneState {
	tool := string(toolOf(info.Cmd))
	provider, argv := ledger.SafeResumeRecipe(tool, info.Cmd, info.Args)
	return ledger.LaneState{
		LaneID: id, Tool: tool, Cwd: info.Cwd, ProviderUUID: provider,
		ResumeArgv: argv, CreatedAtMS: info.CreatedAt,
	}
}

func externalSessionLane(info state.SessionInfo) ledger.LaneState {
	provider, argv := ledger.SafeResumeRecipe(string(info.Tool), info.Cmd, info.Args)
	return ledger.LaneState{
		LaneID: info.ID, Name: info.Name, Tool: string(info.Tool), Cwd: info.Cwd,
		Profile: info.Profile, ConfigDir: info.ConfigDir,
		ProviderUUID: provider, ResumeArgv: argv, CreatedAtMS: info.CreatedAt,
	}
}

func toolOf(command string) state.SessionTool {
	switch strings.ToLower(filepath.Base(command)) {
	case "claude":
		return state.ToolClaude
	case "codex":
		return state.ToolCodex
	default:
		return state.ToolTerminal
	}
}

func (e *Engine) resumeSource(lane ledger.LaneState) (known, exists bool, path string) {
	if lane.ProviderUUID == "" || len(lane.ResumeArgv) == 0 {
		return false, false, ""
	}
	switch lane.Tool {
	case string(state.ToolClaude):
		root := e.options.ClaudeProjectsDir
		if lane.ConfigDir != "" {
			root = filepath.Join(lane.ConfigDir, "projects")
		} else if root == "" {
			var err error
			root, err = watch.ClaudeProjectsDir()
			if err != nil {
				return false, false, ""
			}
		}
		resolution := watch.ResolveClaudeCWD(root, lane.Cwd, lane.ProviderUUID)
		if resolution.Path == "" || filepath.Base(resolution.Path) != lane.ProviderUUID+".jsonl" {
			return true, false, ""
		}
		info, err := os.Stat(resolution.Path)
		return true, err == nil && info.Mode().IsRegular(), resolution.Path
	case string(state.ToolCodex):
		sessionsDir := e.options.CodexSessionsDir
		if lane.ConfigDir != "" {
			sessionsDir = filepath.Join(lane.ConfigDir, "sessions")
		}
		resolution := watch.ResolveCodexRolloutPath(watch.CodexResolveOptions{
			CWD: lane.Cwd, Args: lane.ResumeArgv[1:],
			CreatedAt: time.UnixMilli(lane.CreatedAtMS), SessionsDir: sessionsDir,
		})
		if resolution.Path == "" {
			return true, false, ""
		}
		info, err := os.Stat(resolution.Path)
		return true, err == nil && info.Mode().IsRegular(), resolution.Path
	default:
		return false, false, ""
	}
}

func probeHello(ctx context.Context, socketPath string) (proto.RunnerInfo, error) {
	connection, err := ipc.DialContext(ctx, socketPath)
	if err != nil {
		return proto.RunnerInfo{}, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	frame, err := proto.Read(connection)
	if err != nil {
		return proto.RunnerInfo{}, err
	}
	if frame.Type != proto.Hello {
		return proto.RunnerInfo{}, fmt.Errorf("first frame is %02x, want HELLO", byte(frame.Type))
	}
	var info proto.RunnerInfo
	if err := json.Unmarshal(frame.Payload, &info); err != nil {
		return proto.RunnerInfo{}, err
	}
	if info.ID == "" {
		return proto.RunnerInfo{}, errors.New("HELLO has empty id")
	}
	return info, nil
}

// probeProcess is the platform-neutral liveness signal. It is deliberately
// conservative in the same direction as the session manager's discovery pass:
// an unreadable command line counts as live. Recovery and the manager must not
// disagree about whether a runner still exists, because recovery's answer
// decides whether the user is offered a second runtime for a conversation that
// already has one.
func probeProcess(ctx context.Context, info proto.RunnerInfo) bool {
	if info.PID <= 0 || !processAlive(info.PID) {
		return false
	}
	return runnerProcessMatches(processCommand(ctx, info.PID), info.ID, info.Cmd)
}

// runnerProcessMatches rejects a PID that has been reused by an unrelated
// process. It mirrors the session manager's rule, including its treatment of
// an unknown command as a match.
func runnerProcessMatches(command, id, expectedCommand string) bool {
	if command == "" || strings.Contains(command, id) {
		return true
	}
	// Every runner is the sessions-runner image whatever it hosts, and on
	// Windows only that image path is observable.
	if strings.Contains(strings.ToLower(command), "sessions-runner") {
		return true
	}
	expectedBase := filepath.Base(strings.TrimSpace(expectedCommand))
	return expectedBase != "" && expectedBase != "." && strings.Contains(command, expectedBase)
}

// probeLaunchd answers only on darwin; see ProcessProbe for the signal that
// answers everywhere.
func probeLaunchd(ctx context.Context, id string) (LaunchdStatus, error) {
	if runtime.GOOS != "darwin" {
		return LaunchdStatus{}, nil
	}
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/tech.somewhere.sessions.runner." + id
	output, err := exec.CommandContext(ctx, "launchctl", "print", target).CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return LaunchdStatus{}, nil
		}
		return LaunchdStatus{}, err
	}
	text := strings.ToLower(string(output))
	running := strings.Contains(text, "state = running") || strings.Contains(text, "\n\tpid = ")
	return LaunchdStatus{Loaded: true, Running: running}, nil
}
