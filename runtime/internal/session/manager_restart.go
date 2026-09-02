package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// ErrNotPaused: the session is not one that stayed paused after a reboot.
var ErrNotPaused = errors.New("session is not paused after a reboot")

// WakePaused restarts a session that deliberately stayed paused after a
// reboot and returns it live. A person's first message or first look is the
// trigger; nothing is restarted on a mere listing. The restart cap only
// decides what comes back on its own at boot, not what a person can reach.
func (m *Manager) WakePaused(ctx context.Context, id string) (state.SessionInfo, error) {
	if existing, live := m.registry.Get(id); live && !existing.Info().Unreachable {
		return existing.Info(), nil
	}
	if _, paused := m.PendingRestore(id); !paused {
		return state.SessionInfo{}, fmt.Errorf("%w: %s", ErrNotPaused, id)
	}
	waker, ok := m.launcher.(proto.RunnerWaker)
	if !ok {
		return state.SessionInfo{}, errors.New("this machine cannot restart a paused session in place; resume it with `sessions resume " + id + "`")
	}
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if existing, live := m.registry.Get(id); live && !existing.Info().Unreachable {
		return existing.Info(), nil
	}
	runner, err := waker.Wake(ctx, id)
	if err != nil {
		return state.SessionInfo{}, fmt.Errorf("wake paused session %s: %w", id, err)
	}
	// The runner rewrites its metadata as it starts; a paused runner may have
	// left none behind, in which case what it reports over the socket is the
	// identity.
	metadata, err := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, id+".json"))
	if err != nil {
		metadata = state.RunnerMetadata{Info: runner.Info()}
	}
	session, err := m.registry.RegisterMetadata(ctx, runner, metadata, "")
	if err != nil {
		if existing, live := m.registry.Get(id); live {
			return existing.Info(), nil
		}
		return state.SessionInfo{}, fmt.Errorf("register woken session %s: %w", id, err)
	}
	m.manage(session)
	// The marker said "paused"; the session is live now, whichever launcher
	// brought it back.
	_ = os.Remove(state.For(m.config.RunnerStateDir, id).RestorePending)
	log.Printf("[restore] woke paused session %s on first contact", id)
	return session.Info(), nil
}

const restartRestorePendingReason = "restart-restore-pending"

// PendingRestore reports the durable marker for a session that intentionally
// stayed stopped after a reboot. It is a separate state from unknown, ended,
// and idle; API reads use it to return an actionable error rather than an
// empty successful response.
func (m *Manager) PendingRestore(id string) (state.RestorePending, bool) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return state.RestorePending{}, false
	}
	path := state.For(m.config.RunnerStateDir, id).RestorePending
	pending, err := state.ReadRestorePending(path)
	if err == nil {
		return pending, true
	}
	if os.IsNotExist(err) {
		return state.RestorePending{}, false
	}
	// An unreadable marker still proves restoration did not complete. Keep the
	// session unavailable and give the operator a repair path instead of
	// silently turning corrupt recovery evidence into "unknown session".
	detectedAt := int64(0)
	if info, statErr := os.Stat(path); statErr == nil {
		detectedAt = info.ModTime().UnixMilli()
	}
	return state.RestorePending{
		SessionID: id, DetectedAtMS: detectedAt,
		Reason: "the reboot recovery marker is unreadable: " + err.Error(),
	}, true
}

// withPendingRestores adds reboot-paused sessions to the ordinary live list.
// The metadata is the last durable runtime identity; no field here claims the
// provider is alive. A reachable registry connection always wins.
func (m *Manager) withPendingRestores(infos []state.SessionInfo) []state.SessionInfo {
	indices := make(map[string]int, len(infos))
	for index, info := range infos {
		indices[info.ID] = index
	}
	entries, err := os.ReadDir(m.config.RunnerStateDir)
	if os.IsNotExist(err) {
		return infos
	}
	if err != nil {
		log.Printf("[restore] list paused reboot sessions: %v", err)
		return infos
	}
	const suffix = ".restore-pending.json"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), suffix)
		// A real registry connection is stronger evidence than a stale marker.
		// A durable closed/lost record is not: reboot-pending is the current
		// actionable state and must replace it instead of disappearing behind it.
		if m.registry != nil {
			if existing, live := m.registry.Get(id); live && !existing.Info().Unreachable {
				continue
			}
		}
		pending, exists := m.PendingRestore(id)
		if !exists {
			continue
		}
		metadata, metadataErr := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, id+".json"))
		if metadataErr != nil {
			log.Printf("[restore] read paused session %s metadata: %v", id, metadataErr)
			metadata = m.pausedIdentityFromLedger(id)
		} else if metadata.Info.ID != id {
			log.Printf("[restore] paused session %s metadata belongs to %s", id, metadata.Info.ID)
			metadata = m.pausedIdentityFromLedger(id)
		}
		info := pendingSessionInfo(id, metadata, pending)
		if index, exists := indices[id]; exists {
			infos[index] = info
			continue
		}
		indices[id] = len(infos)
		infos = append(infos, info)
	}
	return infos
}

func pendingSessionInfo(id string, metadata state.RunnerMetadata, pending state.RestorePending) state.SessionInfo {
	lastDataAt := metadata.Info.CreatedAt
	for _, candidate := range []*int64{metadata.LastHumanMessageAt, metadata.LastAgentMessageAt} {
		if candidate != nil && *candidate > lastDataAt {
			lastDataAt = *candidate
		}
	}
	since := pending.DetectedAtMS
	if since == 0 {
		since = lastDataAt
	}
	tool := state.ToolTerminal
	command := strings.ToLower(filepath.Base(metadata.Info.Cmd))
	switch {
	case metadata.Kind == state.KindLane:
		tool = state.ToolLane
	case metadata.Kind == state.KindCodexAppServer || command == "codex":
		tool = state.ToolCodex
	case metadata.Kind == state.KindClaudeStructured || command == "claude":
		tool = state.ToolClaude
	}
	return state.SessionInfo{
		ID: id, Name: metadata.Name, NameSource: metadata.NameSource,
		Description: metadata.Description, DescriptionSource: metadata.DescriptionSource,
		Tags: state.CloneTags(metadata.Tags), Kind: metadata.Kind, SpecPath: metadata.SpecPath,
		Cmd: metadata.Info.Cmd, Args: append([]string(nil), metadata.Info.Args...),
		Cwd: metadata.Info.Cwd, Profile: metadata.Profile, ConfigDir: metadata.ConfigDir,
		Cols: metadata.Info.Cols, Rows: metadata.Info.Rows, CreatedAt: metadata.Info.CreatedAt,
		PID: 0, Tool: tool, LastDataAt: lastDataAt,
		Unreachable: true, UnreachableReason: restartRestorePendingReason, UnreachableSince: &since,
		IdleReason: "needs-recovery", IdleDetail: pending.Reason, IdleSince: &since,
		ConversationID: metadata.Info.ConversationID, RemoteEndpoint: metadata.Info.RemoteEndpoint,
		ClaudeSessionID:        metadata.Info.ClaudeSessionID,
		ContinuedFromHistoryID: metadata.ContinuedFromHistoryID,
		ContinuedFromProvider:  metadata.ContinuedFromProvider,
		ContinuationMode:       metadata.ContinuationMode, ImportedMessageCount: metadata.ImportedMessageCount,
		DisplayParentSessionID: metadata.DisplayParentSessionID, SetAsideAt: metadata.SetAsideAt,
		Pinned: metadata.Pinned, LastHumanMessageAt: metadata.LastHumanMessageAt,
		LastAgentMessageAt: metadata.LastAgentMessageAt, DelegationKind: metadata.DelegationKind,
		Permissions: metadata.Permissions, Lifecycle: metadata.Lifecycle,
	}
}

// pausedIdentityFromLedger rebuilds what a paused session is from its
// creation record when the runner left no metadata behind, so the inbox shows
// its name, folder, and age rather than blanks until it is woken.
func (m *Manager) pausedIdentityFromLedger(id string) state.RunnerMetadata {
	if m.ledgerReader == nil {
		return state.RunnerMetadata{}
	}
	events, err := m.ledgerReader.Events(context.Background(), id)
	if err != nil {
		return state.RunnerMetadata{}
	}
	for _, event := range events {
		if event.Type != ledger.EventCreated {
			continue
		}
		var created ledger.Created
		if json.Unmarshal(event.Payload, &created) != nil {
			continue
		}
		command := created.Tool
		if command == "" || command == string(state.ToolTerminal) {
			command = ""
		}
		return state.RunnerMetadata{
			Info: proto.RunnerInfo{ID: id, Cmd: command, Cwd: created.Cwd, CreatedAt: event.AtMS},
			Name: created.Name, Description: created.Description, Kind: created.Kind,
			Profile: created.Profile, ConfigDir: created.ConfigDir, DelegationKind: created.DelegationKind,
		}
	}
	return state.RunnerMetadata{}
}
