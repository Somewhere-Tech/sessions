package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/mirror"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
)

type Session struct {
	runner proto.Runner
	mirror *mirror.Mirror

	mu           sync.RWMutex
	info         SessionInfo
	outputs      []proto.OutputEvent
	outputSize   int
	nextSeq      uint32
	claude       []json.RawMessage
	claudeBase   int64
	exit         proto.ExitEvent
	subs         map[uint64]chan proto.Event
	nextSubID    uint64
	runnerEvents <-chan proto.Event
	cancelRunner func()
}

type Attachment struct {
	Replay            proto.ReplayWindow
	ClaudeEvents      []json.RawMessage
	ClaudeEventsCount int64
	ClaudeReplayStart int64
	Events            <-chan proto.Event
	Cancel            func()
}

type AttachOptions struct {
	LastSeq             uint32
	ClaudeEventsSince   int64
	IncludeClaudeReplay bool
	InitialReplayCap    int
}

func newSession(ctx context.Context, info proto.RunnerInfo, runner proto.Runner, metadata SessionMetadata) (*Session, error) {
	terminal, err := mirror.NewSize(info.Cols, info.Rows)
	if err != nil {
		return nil, fmt.Errorf("create session mirror: %w", err)
	}
	tool := classifyTool(info.Cmd)
	if metadata.Kind == KindLane {
		tool = SessionTool("lane:" + filepath.Base(info.Cmd))
	} else if metadata.Kind == KindCodexAppServer {
		tool = ToolCodex
	} else if metadata.Kind == KindClaudeStructured {
		tool = ToolClaude
	}
	model, effort, fast := spawnControls(tool, info.Args)
	now := time.Now().UnixMilli()
	session := &Session{
		runner: runner,
		mirror: terminal,
		info: SessionInfo{
			ID: info.ID, Name: metadata.Name, Description: metadata.Description,
			DescriptionSource: metadata.DescriptionSource, Tags: CloneTags(metadata.Tags), Kind: metadata.Kind, SpecPath: metadata.SpecPath,
			Cmd: info.Cmd, Args: append([]string{}, info.Args...),
			Cwd: info.Cwd, Profile: metadata.Profile, ConfigDir: metadata.ConfigDir,
			Cols: info.Cols, Rows: info.Rows, CreatedAt: info.CreatedAt,
			PID: info.PID, RunnerProtocol: info.ProtocolVersion, RunnerVersion: info.RuntimeVersion,
			Tool: tool, LastDataAt: now, OnIdle: metadata.OnIdle,
			IdleReason: IdleReasonNeverStarted, IdleSince: &now,
			Model: model, Effort: effort, Fast: fast,
			ConversationID: info.ConversationID, RemoteEndpoint: info.RemoteEndpoint,
			ClaudeSessionID:        info.ClaudeSessionID,
			ContinuedFromHistoryID: metadata.ContinuedFromHistoryID,
			ContinuedFromProvider:  metadata.ContinuedFromProvider,
			ContinuationMode:       metadata.ContinuationMode,
			ImportedMessageCount:   metadata.ImportedMessageCount,
			DisplayParentSessionID: cloneStringPointer(metadata.DisplayParentSessionID),
			SetAsideAt:             cloneInt64Pointer(metadata.SetAsideAt),
			DelegationKind:         metadata.DelegationKind,
			Permissions:            metadata.Permissions,
			Lifecycle:              metadata.Lifecycle,
		},
		nextSeq: 1,
		subs:    make(map[uint64]chan proto.Event),
	}
	session.runnerEvents, session.cancelRunner = runner.Subscribe()
	replay := runner.Replay(ctx, 0)
	for _, event := range replay.Events {
		session.appendOutputLocked(event)
	}
	for _, event := range replay.Structured {
		replayed := proto.Event{Kind: proto.EventCodex, CodexEvent: event}
		session.recordCodexLocked(&replayed)
	}
	if replay.Current >= session.nextSeq {
		session.nextSeq = replay.Current + 1
	}
	return session, nil
}

func (s *Session) start(onExit func(proto.Event)) {
	go func() {
		defer s.cancelRunner()
		for event := range s.runnerEvents {
			terminal := s.applyEvent(event)
			if terminal {
				onExit(event)
				return
			}
		}
	}()
}

func (s *Session) applyEvent(event proto.Event) bool {
	s.mu.Lock()
	terminal := false
	switch event.Kind {
	case proto.EventOutput:
		if event.Output.Seq == 0 {
			event.Output.Seq = s.nextSeq
		}
		if event.Output.Seq >= s.nextSeq {
			s.appendOutputLocked(event.Output)
			s.info.LastDataAt = time.Now().UnixMilli()
		}
	case proto.EventClaude:
		event.ClaudeActivityAt = s.recordClaudeLocked(&event)
	case proto.EventCodex:
		event.ClaudeActivityAt = s.recordCodexLocked(&event)
		if event.ClaudeActivityAt > s.info.LastDataAt {
			s.info.LastDataAt = event.ClaudeActivityAt
		}
	case proto.EventExit, proto.EventRunnerLost:
		now := time.Now().UnixMilli()
		exit := event.Exit
		if event.Kind == proto.EventRunnerLost && exit.Reason == "" {
			exit.Reason = "runner-lost"
		}
		if exit.Seq == 0 && s.nextSeq > 0 {
			exit.Seq = s.nextSeq - 1
		}
		event.Exit = exit
		s.info.Exited = true
		s.info.Working = false
		s.info.ExitCode = exit.Code
		s.info.ExitSignal = exit.Signal
		s.info.ExitReason = exit.Reason
		s.info.ExitedAt = &now
		s.info.IdleSince = &now
		if (exit.Code != nil && *exit.Code != 0) || event.Kind == proto.EventRunnerLost {
			s.info.IdleReason = IdleReasonFailed
		} else {
			s.info.IdleReason = IdleReasonCompleted
		}
		s.exit = exit
		terminal = true
	}
	for _, subscriber := range s.subs {
		select {
		case subscriber <- event:
		default:
			// A slow client must not stall every other session. WebSocket
			// reconnect/replay repairs dropped output using sequence numbers.
			if terminal {
				// The drain must never block: this runs under the session
				// write lock and the pump goroutine is the channel's only
				// sender, so a consumer that empties the buffer between the
				// failed send and the drain would otherwise wedge Info,
				// Attach, Input, Snapshot, and Registry.List forever. Matches
				// proto.SocketRunner.broadcastLocked.
				select {
				case <-subscriber:
				default:
				}
				subscriber <- event
			}
		}
	}
	if terminal {
		for id, subscriber := range s.subs {
			close(subscriber)
			delete(s.subs, id)
		}
	}
	s.mu.Unlock()
	return terminal
}

func (s *Session) appendOutputLocked(event proto.OutputEvent) {
	if event.At == 0 {
		event.At = time.Now().UnixMilli()
	}
	_, _ = s.mirror.Write([]byte(event.Data))
	s.outputs = append(s.outputs, event)
	s.outputSize += len(event.Data)
	if event.Seq >= s.nextSeq {
		s.nextSeq = event.Seq + 1
	}
	for s.outputSize > defaultEventLogBytes && len(s.outputs) > 1 {
		s.outputSize -= len(s.outputs[0].Data)
		s.outputs = s.outputs[1:]
	}
}

func (s *Session) Info() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := s.info
	info.Args = append([]string{}, s.info.Args...)
	info.Tags = CloneTags(s.info.Tags)
	info.DisplayParentSessionID = cloneStringPointer(s.info.DisplayParentSessionID)
	info.SetAsideAt = cloneInt64Pointer(s.info.SetAsideAt)
	return info
}

func (s *Session) setDisplayParentSessionID(parentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := parentID
	s.info.DisplayParentSessionID = &parent
}

func (s *Session) setSetAsideAt(setAsideAt *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.SetAsideAt = cloneInt64Pointer(setAsideAt)
}

func (s *Session) setTags(tags map[string]string) {
	s.mu.Lock()
	s.info.Tags = CloneTags(tags)
	s.mu.Unlock()
}

func (s *Session) setName(name string) {
	s.mu.Lock()
	s.info.Name = name
	s.mu.Unlock()
}

func (s *Session) setFirstMessageDescription(description string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.DescriptionSource == DescriptionExplicit || s.info.Description != "" {
		return false
	}
	s.info.Description = description
	s.info.DescriptionSource = DescriptionFirstMessage
	return true
}

func (s *Session) Replay(after uint32) proto.ReplayWindow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.replayLocked(after)
}

func (s *Session) replayLocked(after uint32) proto.ReplayWindow {
	oldest := s.nextSeq
	if len(s.outputs) > 0 {
		oldest = s.outputs[0].Seq
	}
	current := uint32(0)
	if s.nextSeq > 0 {
		current = s.nextSeq - 1
	}
	events := make([]proto.OutputEvent, 0, len(s.outputs))
	for _, event := range s.outputs {
		if event.Seq > after {
			events = append(events, event)
		}
	}
	return proto.ReplayWindow{
		Events: events, Gap: len(s.outputs) > 0 && after+1 < oldest,
		Oldest: oldest, Current: current,
	}
}

func (s *Session) Attach(options AttachOptions) Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSubID
	s.nextSubID++
	stream := make(chan proto.Event, 512)
	s.subs[id] = stream

	count := s.claudeBase + int64(len(s.claude))
	start := len(s.claude)
	if options.IncludeClaudeReplay {
		if options.ClaudeEventsSince > 0 {
			start = int(options.ClaudeEventsSince - s.claudeBase)
			if start < 0 {
				start = 0
			}
			if start > len(s.claude) {
				start = len(s.claude)
			}
		} else {
			cap := options.InitialReplayCap
			if cap <= 0 {
				cap = 300
			}
			start = len(s.claude) - cap
			if start < 0 {
				start = 0
			}
		}
	}
	claude := make([]json.RawMessage, len(s.claude)-start)
	for i := range claude {
		claude[i] = append(json.RawMessage(nil), s.claude[start+i]...)
	}
	return Attachment{
		Replay:            s.replayLocked(options.LastSeq),
		ClaudeEvents:      claude,
		ClaudeEventsCount: count,
		ClaudeReplayStart: s.claudeBase + int64(start),
		Events:            stream,
		Cancel: func() {
			s.mu.Lock()
			if existing, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(existing)
			}
			s.mu.Unlock()
		},
	}
}

func (s *Session) Snapshot(_ context.Context, cols int) (string, uint32, error) {
	return s.snapshot(cols, false)
}

// TerminalSnapshot primes an interactive terminal with the bounded history
// retained by the server-side mirror. Other snapshot consumers deliberately
// remain viewport-only so status parsing and responsive reflow are unchanged.
func (s *Session) TerminalSnapshot(_ context.Context) (string, uint32, error) {
	return s.snapshot(0, true)
}

func (s *Session) snapshot(cols int, includeScrollback bool) (string, uint32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seq := uint32(0)
	if s.nextSeq > 0 {
		seq = s.nextSeq - 1
	}
	if s.info.Kind == KindLane {
		const maximum = 64 * 1024
		var output strings.Builder
		for _, event := range s.outputs {
			output.WriteString(event.Data)
		}
		text := output.String()
		if len(text) > maximum {
			text = text[len(text)-maximum:]
		}
		return text, seq, nil
	}
	if s.info.Kind == KindCodexAppServer || s.info.Kind == KindClaudeStructured {
		return structuredSnapshot(s.claude), seq, nil
	}
	if cols > 0 {
		return s.mirror.ReflowTo(cols), seq, nil
	}
	if includeScrollback {
		return s.mirror.SerializeANSIWithScrollback(), seq, nil
	}
	return s.mirror.SerializeANSI(), seq, nil
}

func (s *Session) Input(ctx context.Context, data string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	exited := s.info.Exited
	return !exited && s.runner.Input(ctx, data) == nil
}

// ConfigureModel updates the defaults used by the next structured-provider
// turn. Holding the session write lock across the runner frame orders this
// change before any later user input through Session.Input.
func (s *Session) ConfigureModel(ctx context.Context, model, effort string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.Exited {
		return ErrSessionEnded
	}
	if s.info.Working {
		return fmt.Errorf("%w; wait for the turn to finish", ErrSessionWorking)
	}
	if s.info.RunnerProtocol < 2 {
		return fmt.Errorf("%w: runner protocol v%d cannot change models live; update Sessions and start or resume this conversation with the current runtime", ErrRunnerProtocol, s.info.RunnerProtocol)
	}
	if err := s.runner.ConfigureModel(ctx, proto.ModelControl{Model: model, Effort: effort}); err != nil {
		return fmt.Errorf("configure runner model: %w", err)
	}
	s.info.Args = withModelControls(s.info.Tool, s.info.Args, model, effort)
	s.info.Model = model
	s.info.Effort = effort
	return nil
}

func withModelControls(tool SessionTool, args []string, model, effort string) []string {
	result := providerargs.WithValue(args, model, providerargs.ModelFlags()...)
	if tool == ToolCodex {
		return providerargs.WithConfigValue(result, providerargs.CodexEffortKey, effort)
	}
	return providerargs.WithValue(result, effort, providerargs.ClaudeEffortFlags()...)
}

func (s *Session) Resize(ctx context.Context, cols, rows int) bool {
	s.mu.RLock()
	exited := s.info.Exited
	s.mu.RUnlock()
	if exited || s.runner.Resize(ctx, cols, rows) != nil {
		return false
	}
	s.mu.Lock()
	if err := s.mirror.Resize(cols, rows); err != nil {
		s.mu.Unlock()
		return false
	}
	s.info.Cols = cols
	s.info.Rows = rows
	s.mu.Unlock()
	return true
}

func (s *Session) Kill(ctx context.Context) bool {
	return s.RequestKill(ctx) == nil
}

func (s *Session) RequestKill(ctx context.Context) error {
	s.mu.RLock()
	exited := s.info.Exited
	s.mu.RUnlock()
	if exited {
		return errors.New("session already exited")
	}
	if err := s.runner.Kill(ctx); err != nil {
		return fmt.Errorf("kill runner: %w", err)
	}
	return nil
}

// SetWorking is the single synchronized mutation point used by the session
// runtime's activity classifier. It returns the previous value and whether the
// runner has already exited so transition side effects can be gated safely.
func (s *Session) SetWorking(working bool) (previous bool, exited bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous = s.info.Working
	exited = s.info.Exited
	if !exited {
		s.info.Working = working
		if working {
			s.info.IdleReason = ""
			s.info.IdleDetail = ""
			s.info.IdleSince = nil
		}
	}
	return previous, exited
}

// SetIdleResult publishes the last useful outcome as additive session state.
// LastSummary deliberately survives the next working transition so operators
// can still see the most recently completed result while a follow-up runs.
func (s *Session) SetIdleResult(reason, detail, summary string, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.Exited {
		return
	}
	s.info.IdleReason = reason
	s.info.IdleDetail = detail
	s.info.IdleSince = &at
	if summary != "" {
		s.info.LastSummary = summary
	}
}

// ClearIdleResult records that new input invalidated the previous idle outcome.
// LastSummary remains available as the last completed result, but a scheduled
// task cleanup must no longer treat the session as completed.
func (s *Session) ClearIdleResult() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.Exited {
		return
	}
	s.info.IdleReason = ""
	s.info.IdleDetail = ""
	s.info.IdleSince = nil
}

// RecordClaudeEvent adds one watcher-derived structured event to the same log
// and subscriber stream used for runner-provided events.
func (s *Session) RecordClaudeEvent(event json.RawMessage) {
	s.applyEvent(proto.Event{Kind: proto.EventClaude, ClaudeEvent: append(json.RawMessage(nil), event...)})
}

// RecordCodexEvent adds one app-server event to the canonical structured
// history without involving the Codex rollout watcher.
func (s *Session) RecordCodexEvent(event json.RawMessage) {
	s.applyEvent(proto.Event{Kind: proto.EventCodex, CodexEvent: append(json.RawMessage(nil), event...)})
}

// ClaudeEventLog returns a defensive copy for completion-summary extraction.
func (s *Session) ClaudeEventLog() []json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	events := make([]json.RawMessage, len(s.claude))
	for i := range events {
		events[i] = append(json.RawMessage(nil), s.claude[i]...)
	}
	return events
}

func (s *Session) EventsWindow(since, tail, before *int64) ClaudeEventsWindow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	length := int64(len(s.claude))
	total := s.claudeBase + length
	end := length
	if before != nil && *before >= 0 {
		end = clamp(*before-s.claudeBase, 0, length)
	}
	start := int64(0)
	if since != nil && *since >= 0 {
		start = clamp(*since-s.claudeBase, 0, end)
	}
	if tail != nil && *tail > 0 {
		candidate := end - *tail
		if candidate < 0 {
			candidate = 0
		}
		if candidate > start {
			start = candidate
		}
	}
	if start > end {
		start = end
	}
	events := make([]json.RawMessage, end-start)
	for i := range events {
		events[i] = append(json.RawMessage(nil), s.claude[start+int64(i)]...)
	}
	return ClaudeEventsWindow{
		Events: events, NextIndex: total, TotalCount: total,
		StartIndex: s.claudeBase + start, EndIndex: s.claudeBase + end,
	}
}

func (s *Session) ClaudeEventCount() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.claudeBase + int64(len(s.claude))
}

func (s *Session) TerminalState() (bool, proto.ExitEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info.Exited, s.exit
}

func (s *Session) Close() error {
	s.cancelRunner()
	return s.mirror.Close()
}

func clamp(value, minimum, maximum int64) int64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
