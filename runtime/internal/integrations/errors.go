package integrations

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type ErrorInput struct {
	Kind      string
	SessionID string
	Summary   string
	Detail    string
	TS        string
}

type ErrorEvent struct {
	Seq       uint64 `json:"seq"`
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	Summary   string `json:"summary"`
	Detail    string `json:"detail"`
	Machine   string `json:"machine"`
}

type ErrorsResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Errors        []ErrorEvent `json:"errors"`
	NextSeq       uint64       `json:"nextSeq"`
}

type ErrorRecorder struct {
	path    string
	machine string
	now     func() time.Time

	mu          sync.Mutex
	initialized bool
	events      []ErrorEvent
	nextSeq     uint64
}

func NewErrorRecorder(path, machine string, now func() time.Time) *ErrorRecorder {
	if now == nil {
		now = time.Now
	}
	return &ErrorRecorder{path: path, machine: machine, now: now}
}

func (r *ErrorRecorder) Emit(input ErrorInput) (ErrorEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.initializeLocked(); err != nil {
		return ErrorEvent{}, err
	}
	kind := strings.TrimSpace(input.Kind)
	summary := strings.TrimSpace(input.Summary)
	if kind == "" || summary == "" {
		return ErrorEvent{}, errors.New("error kind and summary are required")
	}
	ts := input.TS
	if ts == "" {
		ts = r.now().UTC().Format(time.RFC3339Nano)
	} else {
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return ErrorEvent{}, fmt.Errorf("error timestamp must be RFC 3339: %w", err)
		}
		ts = parsed.UTC().Format(time.RFC3339Nano)
	}
	event := ErrorEvent{
		Seq: r.nextSeq + 1, TS: ts, Kind: kind,
		SessionID: strings.TrimSpace(input.SessionID), Summary: summary,
		Detail: input.Detail, Machine: r.machine,
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return ErrorEvent{}, err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return ErrorEvent{}, fmt.Errorf("create error event directory: %w", err)
	}
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return ErrorEvent{}, fmt.Errorf("open error event log: %w", err)
	}
	writeErr := error(nil)
	if err := os.Chmod(r.path, 0o600); err != nil {
		writeErr = err
	} else if _, err := file.Write(append(encoded, '\n')); err != nil {
		writeErr = err
	} else {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return ErrorEvent{}, fmt.Errorf("append error event: %w", err)
	}
	r.events = append(r.events, event)
	r.nextSeq = event.Seq
	return event, nil
}

func (r *ErrorRecorder) Feed(since uint64) (ErrorsResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.initializeLocked(); err != nil {
		return ErrorsResponse{}, err
	}
	events := make([]ErrorEvent, 0)
	for _, event := range r.events {
		if event.Seq > since {
			events = append(events, event)
		}
	}
	next := r.nextSeq
	if since > next {
		next = since
	}
	return ErrorsResponse{SchemaVersion: SchemaVersion, Errors: events, NextSeq: next}, nil
}

func (r *ErrorRecorder) initializeLocked() error {
	if r.initialized {
		return nil
	}
	file, err := os.Open(r.path)
	if errors.Is(err, os.ErrNotExist) {
		r.initialized = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("open error event log: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	lineNumber := 0
	loaded := make([]ErrorEvent, 0)
	var nextSeq uint64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				var event ErrorEvent
				if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
					return fmt.Errorf("decode error event line %d: %w", lineNumber, err)
				}
				if event.Seq == 0 || event.Kind == "" || event.Summary == "" {
					return fmt.Errorf("decode error event line %d: invalid event", lineNumber)
				}
				loaded = append(loaded, event)
				if event.Seq > nextSeq {
					nextSeq = event.Seq
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read error event log: %w", readErr)
		}
	}
	r.events = loaded
	r.nextSeq = nextSeq
	r.initialized = true
	return nil
}

type ServiceOptions struct {
	StateDir                string
	RunnerStateDir          string
	ClaudeProjectsDir       string
	CodexSessionsDir        string
	Machine                 string
	Now                     func() time.Time
	DiscoverProviderHistory bool
}

type Service struct {
	history  *HistoryStore
	recorder *ErrorRecorder

	observedMu          sync.Mutex
	observedInitialized bool
	observedExits       map[string]struct{}
	trackedSessions     map[string]struct{}
}

func NewService(options ServiceOptions) *Service {
	machine := options.Machine
	if machine == "" {
		machine, _ = os.Hostname()
	}
	if machine == "" {
		machine = "unknown"
	}
	return &Service{
		history: NewHistoryStore(HistoryOptions{
			RunnerStateDir: options.RunnerStateDir, ClaudeProjectsDir: options.ClaudeProjectsDir,
			CodexSessionsDir: options.CodexSessionsDir, Machine: machine, Now: options.Now,
			DiscoverProviderHistory: options.DiscoverProviderHistory,
		}),
		recorder:        NewErrorRecorder(filepath.Join(options.StateDir, "errors.jsonl"), machine, options.Now),
		observedExits:   make(map[string]struct{}),
		trackedSessions: make(map[string]struct{}),
	}
}

func (s *Service) History(live []state.SessionInfo) (HistoryResponse, error) {
	return s.history.List(live)
}

func (s *Service) SearchSessions(live []state.SessionInfo) ([]HistorySession, error) {
	return s.history.SearchSessions(live)
}

func (s *Service) LookupHistory(live []state.SessionInfo, id string) (HistorySession, error) {
	return s.history.Lookup(live, id)
}

func (s *Service) Transcript(live []state.SessionInfo, id string) (TranscriptResponse, error) {
	return s.history.Transcript(live, id)
}

func (s *Service) TranscriptLimited(live []state.SessionInfo, id string, maxBytes int64) (TranscriptResponse, error) {
	return s.history.TranscriptLimited(live, id, maxBytes)
}

func (s *Service) TranscriptPreview(live []state.SessionInfo, id string, maxBytes int64, maxMessages int) (TranscriptResponse, error) {
	return s.history.TranscriptPreview(live, id, maxBytes, maxMessages)
}

func (s *Service) TranscriptWindow(live []state.SessionInfo, id string, options TranscriptWindowOptions) (TranscriptResponse, error) {
	return s.history.TranscriptWindow(live, id, options)
}

func (s *Service) TranscriptLimitedContext(ctx context.Context, live []state.SessionInfo, id string, maxBytes int64) (TranscriptResponse, error) {
	return s.history.TranscriptLimitedContext(ctx, live, id, maxBytes)
}

func (s *Service) Raw(live []state.SessionInfo, id string) ([]byte, error) {
	return s.history.Raw(live, id)
}

func (s *Service) Emit(input ErrorInput) (ErrorEvent, error) { return s.recorder.Emit(input) }

func (s *Service) ErrorFeed(since uint64) (ErrorsResponse, error) { return s.recorder.Feed(since) }

func (s *Service) ObserveFailures(sessions []state.SessionInfo) error {
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if err := s.initializeObservedLocked(); err != nil {
		return err
	}
	for _, session := range sessions {
		if err := s.observeFailureLocked(session); err != nil {
			return err
		}
	}
	return nil
}

// TrackSession attaches a read-only subscriber to a live session so a nonzero
// exit is persisted at the terminal event, even if the next feed poll arrives
// after the daemon's exited-session grace window.
func (s *Service) TrackSession(session *state.Session) error {
	info := session.Info()
	s.observedMu.Lock()
	if err := s.initializeObservedLocked(); err != nil {
		s.observedMu.Unlock()
		return err
	}
	if _, exists := s.trackedSessions[info.ID]; exists {
		s.observedMu.Unlock()
		return nil
	}
	s.trackedSessions[info.ID] = struct{}{}
	s.observedMu.Unlock()

	if terminal, exit := session.TerminalState(); terminal {
		return s.observeTerminal(session.Info(), exit)
	}
	attachment := session.Attach(state.AttachOptions{})
	if terminal, exit := session.TerminalState(); terminal {
		attachment.Cancel()
		return s.observeTerminal(session.Info(), exit)
	}
	go func() {
		defer attachment.Cancel()
		for event := range attachment.Events {
			if event.Kind != proto.EventExit && event.Kind != proto.EventRunnerLost {
				continue
			}
			if err := s.observeTerminal(session.Info(), event.Exit); err != nil {
				log.Printf("[integrations] record runner failure: %v", err)
			}
			return
		}
	}()
	return nil
}

func (s *Service) initializeObservedLocked() error {
	if s.observedInitialized {
		return nil
	}
	feed, err := s.recorder.Feed(0)
	if err != nil {
		return err
	}
	for _, event := range feed.Errors {
		if (event.Kind == "runner_exit" || event.Kind == "runner_lost") && event.SessionID != "" {
			s.observedExits[event.SessionID] = struct{}{}
		}
	}
	s.observedInitialized = true
	return nil
}

func (s *Service) observeFailure(session state.SessionInfo) error {
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if err := s.initializeObservedLocked(); err != nil {
		return err
	}
	return s.observeFailureLocked(session)
}

func (s *Service) observeTerminal(session state.SessionInfo, exit proto.ExitEvent) error {
	if exit.Reason != "runner-lost" {
		return s.observeFailure(session)
	}
	s.observedMu.Lock()
	defer s.observedMu.Unlock()
	if err := s.initializeObservedLocked(); err != nil {
		return err
	}
	if _, exists := s.observedExits[session.ID]; exists {
		return nil
	}
	input := ErrorInput{
		Kind: "runner_lost", SessionID: session.ID,
		Summary: "runner connection lost",
		Detail:  fmt.Sprintf("reason=%s tool=%s cwd=%s", exit.Reason, session.Tool, session.Cwd),
	}
	if session.ExitedAt != nil && *session.ExitedAt > 0 {
		input.TS = time.UnixMilli(*session.ExitedAt).UTC().Format(time.RFC3339Nano)
	}
	if _, err := s.recorder.Emit(input); err != nil {
		return err
	}
	s.observedExits[session.ID] = struct{}{}
	return nil
}

func (s *Service) observeFailureLocked(session state.SessionInfo) error {
	if !abnormalExit(session) {
		return nil
	}
	if _, exists := s.observedExits[session.ID]; exists {
		return nil
	}
	input := ErrorInput{
		Kind: "runner_exit", SessionID: session.ID,
		Summary: runnerExitSummary(session), Detail: runnerExitDetail(session),
	}
	if session.ExitedAt != nil && *session.ExitedAt > 0 {
		input.TS = time.UnixMilli(*session.ExitedAt).UTC().Format(time.RFC3339Nano)
	}
	if _, err := s.recorder.Emit(input); err != nil {
		return err
	}
	s.observedExits[session.ID] = struct{}{}
	return nil
}

func abnormalExit(session state.SessionInfo) bool {
	return session.Exited && ((session.ExitCode != nil && *session.ExitCode != 0) ||
		(session.ExitSignal != nil && *session.ExitSignal != ""))
}

func runnerExitSummary(session state.SessionInfo) string {
	if session.ExitCode != nil {
		return fmt.Sprintf("runner exited with code %d", *session.ExitCode)
	}
	return "runner exited from signal"
}

func runnerExitDetail(session state.SessionInfo) string {
	code, signal := "unknown", ""
	if session.ExitCode != nil {
		code = fmt.Sprint(*session.ExitCode)
	}
	if session.ExitSignal != nil {
		signal = *session.ExitSignal
	}
	return fmt.Sprintf("exit_code=%s signal=%s tool=%s cwd=%s", code, signal, session.Tool, session.Cwd)
}
