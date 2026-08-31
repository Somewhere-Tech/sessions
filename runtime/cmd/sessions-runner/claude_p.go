package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// claudeStructuredRunner is the durable socket/history owner for a structured
// Claude conversation. Each user turn is a separate claude -p process; the
// persisted Claude UUID is the continuity and restart boundary.
type claudeStructuredRunner struct {
	cfg       config
	paths     state.Paths
	createdAt int64
	client    *claudep.Client
	logger    *log.Logger

	sessionID    string
	initialized  bool
	listener     net.Listener
	historyFile  *os.File
	continuation *state.ContinuationContext

	ctx    context.Context
	cancel context.CancelFunc
	done   chan int

	streamMu sync.Mutex
	mu       sync.Mutex
	clients  map[*client]struct{}
	history  []json.RawMessage
	composer strings.Builder
	active   bool

	shutdownOnce sync.Once
}

func runClaudeStructured(cfg config, paths state.Paths, logger *log.Logger) int {
	ctx, cancel := context.WithCancel(context.Background())
	host := &claudeStructuredRunner{
		cfg: cfg, paths: paths, logger: logger, ctx: ctx, cancel: cancel,
		done: make(chan int, 1), clients: make(map[*client]struct{}),
	}
	if err := host.start(); err != nil {
		logger.Printf("structured Claude host failed: %v", err)
		removeRestartState(paths)
		cancel()
		return 1
	}
	signal.Ignore(os.Interrupt, syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	go func() {
		select {
		case <-term:
			host.shutdownForHostExit(false, 1)
		case <-ctx.Done():
		}
	}()
	return <-host.done
}

func (r *claudeStructuredRunner) start() error {
	metadata, _ := state.ReadRunnerMetadata(r.paths.Meta)
	r.createdAt = metadata.Info.CreatedAt
	if r.createdAt == 0 {
		r.createdAt = time.Now().UnixMilli()
	}
	r.sessionID = metadata.Info.ClaudeSessionID
	if r.sessionID == "" {
		r.sessionID = providerargs.ClaudeSessionID(r.cfg.args)
	}
	if r.sessionID == "" {
		var err error
		r.sessionID, err = claudep.NewSessionID()
		if err != nil {
			return fmt.Errorf("generate Claude session id: %w", err)
		}
	}
	created, err := claudep.NewClient(claudep.Options{ClaudePath: r.cfg.cmd})
	if err != nil {
		return err
	}
	r.client = created
	if err := r.openHistory(); err != nil {
		return err
	}
	if providerargs.ClaudeResumeID(r.cfg.args) != "" {
		r.initialized = true
	}
	if r.historyWorking() {
		recovery, _ := claudep.FailureHistoryEvent(r.sessionID, errors.New("structured runner restarted during an unfinished Claude turn"), time.Now())
		r.appendStructured(recovery)
	}
	if err := r.prepareContinuation(); err != nil {
		r.closeHistory()
		return err
	}
	if err := r.writeMetadata(); err != nil {
		r.closeHistory()
		return err
	}
	listener, err := ipc.Listen(r.paths.Socket)
	if err != nil {
		r.closeHistory()
		return err
	}
	r.listener = listener
	go r.acceptLoop()
	return nil
}

func (r *claudeStructuredRunner) prepareContinuation() error {
	continuation, err := state.ReadContinuation(r.paths.Continuation)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read continuation: %w", err)
	}
	if continuation.DestinationProvider != "claude" || continuation.Mode != state.ContinuationLinkedSearch {
		return fmt.Errorf("continuation mode %q is not valid for Claude", continuation.Mode)
	}
	if continuation.ProviderContext == "applying" {
		if r.initialized {
			continuation.ProviderContext = "applied"
		} else {
			continuation.ProviderContext = ""
		}
	}
	if !continuation.LocalHistoryReady {
		for _, message := range continuation.Messages {
			raw, encodeErr := claudep.ImportedHistoryEvent(
				r.sessionID, message.Role, message.Text, continuation.SourceHistoryID,
				continuationTime(message.Timestamp),
			)
			if encodeErr == nil {
				r.appendStructured(raw)
			}
		}
		continuation.LocalHistoryReady = true
	}
	if err := state.WriteContinuation(r.paths.Continuation, continuation); err != nil {
		return fmt.Errorf("prepare Claude continuation: %w", err)
	}
	r.continuation = &continuation
	return nil
}

func (r *claudeStructuredRunner) historyWorking() bool {
	working := false
	for _, raw := range r.history {
		if claudep.HistoryInitialized(raw) {
			r.initialized = true
		}
		if value, ok := claudep.HistoryLifecycle(raw); ok {
			working = value
		}
	}
	return working
}

func (r *claudeStructuredRunner) writeMetadata() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeMetadataLocked()
}

func (r *claudeStructuredRunner) writeMetadataLocked() error {
	return state.WriteRunnerMetadata(r.paths.Meta, state.Metadata{
		ID: r.cfg.id, Name: r.cfg.name, Description: r.cfg.description,
		DescriptionSource: r.cfg.descriptionSource, Kind: r.cfg.kind, SpecPath: r.cfg.specPath,
		Profile: r.cfg.profile, ConfigDir: r.cfg.configDir,
		Cmd: r.cfg.cmd, Args: r.cfg.args, Cwd: r.cfg.cwd,
		Cols: r.cfg.cols, Rows: r.cfg.rows, CreatedAt: r.createdAt,
		PID: os.Getpid(), SockPath: r.paths.Socket, ClaudeSessionID: r.sessionID,
		ContinuedFromHistoryID: continuationHistoryID(r.continuation),
		ContinuedFromProvider:  continuationSourceProvider(r.continuation),
		ContinuationMode:       continuationMode(r.continuation),
		ImportedMessageCount:   continuationMessageCount(r.continuation),
	})
}

func (r *claudeStructuredRunner) openHistory() error {
	file, err := os.OpenFile(r.paths.ClaudeP, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := os.Chmod(r.paths.ClaudeP, 0o600); err != nil {
		_ = file.Close()
		return err
	}
	history, err := readStructuredHistoryTail(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	r.history = history
	r.historyFile = file
	return nil
}

func (r *claudeStructuredRunner) closeHistory() {
	if r.historyFile != nil {
		_ = r.historyFile.Close()
	}
}

func (r *claudeStructuredRunner) acceptLoop() {
	for {
		connection, err := r.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				r.logger.Printf("structured Claude socket accept failed: %v", err)
			}
			return
		}
		go r.serveClient(connection)
	}
}

func (r *claudeStructuredRunner) serveClient(connection net.Conn) {
	c := newClient(connection)
	r.streamMu.Lock()
	r.mu.Lock()
	r.clients[c] = struct{}{}
	h := hello{
		ID: r.cfg.id, Cmd: r.cfg.cmd, Args: r.cfg.args, Cwd: r.cfg.cwd,
		Cols: r.cfg.cols, Rows: r.cfg.rows, CreatedAt: r.createdAt,
		PID: os.Getpid(), ProtocolVersion: proto.ProtocolVersion, RuntimeVersion: version,
		ClaudeSessionID: r.sessionID,
	}
	r.mu.Unlock()
	payload, err := json.Marshal(h)
	if err == nil {
		err = c.write(proto.Hello, payload)
	}
	r.streamMu.Unlock()
	if err != nil {
		c.close()
	}
	defer func() {
		c.close()
		r.mu.Lock()
		delete(r.clients, c)
		r.mu.Unlock()
	}()
	for {
		frame, err := proto.Read(connection)
		if err != nil {
			return
		}
		if err := r.handleFrame(c, frame); err != nil {
			return
		}
	}
}

func (r *claudeStructuredRunner) handleFrame(c *client, frame proto.Frame) error {
	switch frame.Type {
	case proto.Input:
		r.handleInput(string(frame.Payload))
	case proto.ModelReq:
		control, err := proto.DecodeModelControl(frame.Payload)
		if err == nil {
			err = r.configureModel(control)
		}
		result := proto.ModelControlResult{}
		if err != nil {
			result.Error = err.Error()
			r.logger.Printf("reject model control: %v", err)
		}
		payload, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return marshalErr
		}
		return c.write(proto.ModelRes, payload)
	case proto.Resize:
		return nil
	case proto.SnapshotReq:
		r.streamMu.Lock()
		snapshot := []byte(r.snapshot())
		if len(snapshot)+1 > proto.MaxFrameLen {
			snapshot = snapshot[len(snapshot)-(proto.MaxFrameLen-1):]
		}
		err := c.write(proto.SnapshotRes, snapshot)
		r.streamMu.Unlock()
		return err
	case proto.ReplayReq:
		r.streamMu.Lock()
		r.mu.Lock()
		history := cloneStructured(r.history)
		r.mu.Unlock()
		for _, event := range history {
			if err := c.write(proto.Structured, event); err != nil {
				r.streamMu.Unlock()
				return err
			}
		}
		err := c.write(proto.ReplayDone, nil)
		r.streamMu.Unlock()
		return err
	case proto.Kill:
		go r.shutdown(true, 0)
	}
	return nil
}

func (r *claudeStructuredRunner) configureModel(control proto.ModelControl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active {
		return errors.New("Claude turn is active")
	}
	previous := append([]string(nil), r.cfg.args...)
	r.cfg.args = providerargs.WithValue(r.cfg.args, control.Model, providerargs.ModelFlags()...)
	r.cfg.args = providerargs.WithValue(r.cfg.args, control.Effort, providerargs.ClaudeEffortFlags()...)
	if err := r.writeMetadataLocked(); err != nil {
		r.cfg.args = previous
		return err
	}
	return nil
}

func (r *claudeStructuredRunner) handleInput(data string) {
	r.mu.Lock()
	var rejected []json.RawMessage
	parts := strings.Split(data, "\r")
	for index, part := range parts {
		r.composer.WriteString(part)
		if index == len(parts)-1 {
			continue
		}
		text := cleanComposerInput(r.composer.String())
		r.composer.Reset()
		if text == "" {
			continue
		}
		if r.active {
			event, err := claudep.InputRejectedEvent(
				r.sessionID,
				"Claude is still working. This message was not sent or queued; send it again after the turn finishes.",
				time.Now(),
			)
			if err == nil {
				rejected = append(rejected, event)
			}
			continue
		}
		r.active = true
		go r.runTurn(text)
	}
	r.mu.Unlock()
	for _, event := range rejected {
		r.appendStructured(event)
	}
}

func (r *claudeStructuredRunner) runTurn(text string) {
	defer func() {
		r.mu.Lock()
		r.active = false
		r.mu.Unlock()
	}()
	user, _ := claudep.UserHistoryEvent(r.sessionID, text, time.Now())
	r.appendStructured(user)
	started, _ := claudep.TurnStartedEvent(r.sessionID, time.Now())
	r.appendStructured(started)
	r.mu.Lock()
	resume := r.initialized
	cfg := r.cfg
	cfg.args = append([]string(nil), r.cfg.args...)
	continuation := r.continuation
	r.mu.Unlock()
	applyContinuation := continuation != nil && continuation.ProviderContext != "applied"
	if applyContinuation {
		updated := *continuation
		updated.ProviderContext = "applying"
		if err := state.WriteContinuation(r.paths.Continuation, updated); err != nil {
			r.recordTurnFailure(fmt.Errorf("prepare linked continuation: %w", err))
			return
		}
		cfg.args = withContinuationSystemPrompt(cfg.args, continuationBridge(updated))
	}
	stream, err := r.client.SendTurn(r.ctx, text, claudep.TurnOptions{
		CWD: cfg.cwd, SessionID: r.sessionID, Resume: resume,
		Model: providerargs.Value(cfg.args, providerargs.ModelFlags()...), ExtraArgs: cfg.args,
	})
	if err != nil {
		if applyContinuation {
			updated := *continuation
			updated.ProviderContext = ""
			_ = state.WriteContinuation(r.paths.Continuation, updated)
		}
		r.recordTurnFailure(structuredProfileLoginHint(err, cfg.profile))
		return
	}
	if applyContinuation {
		updated := *continuation
		updated.ProviderContext = "applied"
		if err := state.WriteContinuation(r.paths.Continuation, updated); err == nil {
			r.mu.Lock()
			r.continuation = &updated
			r.mu.Unlock()
		}
	}
	completed := false
	for event := range stream.Events {
		if claudep.HistoryInitialized(event.Raw) {
			r.mu.Lock()
			r.initialized = true
			r.mu.Unlock()
		}
		if event.Type == "result" {
			completed = true
		}
		r.appendStructured(event.Raw)
	}
	_, err = stream.Result(r.ctx)
	if err != nil && !errors.Is(err, context.Canceled) && (!completed || cfg.profile != "") {
		r.recordTurnFailure(structuredProfileLoginHint(err, cfg.profile))
	}
}

func structuredProfileLoginHint(err error, profile string) error {
	if err == nil || profile == "" {
		return err
	}
	return fmt.Errorf("%w; new profile: open a regular PTY session with --profile %s once to log in", err, profile)
}

func (r *claudeStructuredRunner) recordTurnFailure(err error) {
	raw, encodeErr := claudep.FailureHistoryEvent(r.sessionID, err, time.Now())
	if encodeErr == nil {
		r.appendStructured(raw)
	}
}

func (r *claudeStructuredRunner) appendStructured(raw json.RawMessage) {
	if len(raw) == 0 || len(raw)+1 > proto.MaxFrameLen {
		return
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if _, err := r.historyFile.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
		r.logger.Printf("append structured Claude history failed: %v", err)
	}
	r.mu.Lock()
	r.history = retainStructuredEvent(r.history, raw)
	clients := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.enqueue(proto.Structured, raw)
	}
}

func (r *claudeStructuredRunner) snapshot() string {
	r.mu.Lock()
	history := cloneStructured(r.history)
	r.mu.Unlock()
	return structuredTranscript(history)
}

func (r *claudeStructuredRunner) shutdown(permanent bool, code int) {
	r.shutdownWithRestartPolicy(permanent, code, false)
}

func (r *claudeStructuredRunner) shutdownForHostExit(permanent bool, code int) {
	r.shutdownWithRestartPolicy(permanent, code, true)
}

func (r *claudeStructuredRunner) shutdownWithRestartPolicy(permanent bool, code int, preserveRestartPermit bool) {
	r.shutdownOnce.Do(func() {
		r.cancel()
		r.streamMu.Lock()
		if r.listener != nil {
			_ = r.listener.Close()
		}
		_ = ipc.Remove(r.paths.Socket)
		if !preserveRestartPermit {
			removeRestartState(r.paths)
		}
		r.mu.Lock()
		exitCode := code
		exit := exitInfo{Code: &exitCode}
		clients := make([]*client, 0, len(r.clients))
		for c := range r.clients {
			clients = append(clients, c)
		}
		r.mu.Unlock()
		payload, _ := json.Marshal(exit)
		for _, c := range clients {
			_ = c.write(proto.Exit, payload)
			c.close()
		}
		r.closeHistory()
		if permanent {
			_ = os.Remove(r.paths.Meta)
			_ = os.Remove(r.paths.ClaudeP)
		}
		r.streamMu.Unlock()
		r.done <- code
	})
}
