package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const structuredScannerBuffer = 8 * 1024 * 1024

// codexAppRunner is the durable launchd host for one structured Codex
// conversation. The app-server owns the conversation; this process owns the
// client connection, normalized history, and canonical runner socket.
type codexAppRunner struct {
	cfg       config
	paths     state.Paths
	createdAt int64
	client    *codexapp.Client
	logger    *log.Logger

	conversationID string
	remoteEndpoint string
	listener       net.Listener
	historyFile    *os.File
	continuation   *state.ContinuationContext

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

func runCodexAppServer(cfg config, paths state.Paths, logger *log.Logger) int {
	ctx, cancel := context.WithCancel(context.Background())
	host := &codexAppRunner{
		cfg: cfg, paths: paths, logger: logger, ctx: ctx, cancel: cancel,
		done: make(chan int, 1), clients: make(map[*client]struct{}),
	}
	if err := host.start(); err != nil {
		logger.Printf("codex app-server host failed: %v", err)
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
			host.shutdown(false, 1)
		case <-ctx.Done():
		}
	}()
	return <-host.done
}

func (r *codexAppRunner) start() error {
	metadata, _ := state.ReadRunnerMetadata(r.paths.Meta)
	r.createdAt = metadata.Info.CreatedAt
	if r.createdAt == 0 {
		r.createdAt = time.Now().UnixMilli()
	}
	if continuation, readErr := state.ReadContinuation(r.paths.Continuation); readErr == nil {
		if continuation.DestinationProvider != "codex" || continuation.Mode != state.ContinuationNativeImport {
			return fmt.Errorf("continuation mode %q is not valid for Codex", continuation.Mode)
		}
		r.continuation = &continuation
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read continuation: %w", readErr)
	}

	options := codexapp.Options{}
	if socketPath := strings.TrimSpace(os.Getenv("SESSIONS_CODEX_APP_SERVER_SOCKET")); socketPath != "" {
		options.SkipDaemonStart = true
		options.SocketPath = socketPath
	}
	client, err := codexapp.NewClient(r.ctx, options)
	if err != nil {
		return err
	}
	r.client = client
	conversationOptions := codexConversationOptions(r.cfg)
	if r.continuation != nil {
		conversationOptions.DeveloperInstructions = codexContinuationInstructions(*r.continuation)
	}
	r.conversationID = metadata.Info.ConversationID
	if r.conversationID == "" {
		r.conversationID, err = client.NewConversation(r.ctx, conversationOptions)
	} else {
		_, err = client.ResumeConversation(r.ctx, r.conversationID, conversationOptions)
	}
	if err != nil {
		_ = client.Close()
		return err
	}
	r.remoteEndpoint = client.RemoteEndpoint()

	if err := r.openHistory(); err != nil {
		_ = client.Close()
		return err
	}
	if err := r.prepareContinuation(); err != nil {
		r.closeHistory()
		_ = client.Close()
		return err
	}
	if err := r.writeMetadata(); err != nil {
		r.closeHistory()
		_ = client.Close()
		return err
	}

	listener, err := ipc.Listen(r.paths.Socket)
	if err != nil {
		r.closeHistory()
		_ = client.Close()
		return err
	}
	r.listener = listener
	go r.acceptLoop()
	return nil
}

func (r *codexAppRunner) prepareContinuation() error {
	continuation, err := state.ReadContinuation(r.paths.Continuation)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read continuation: %w", err)
	}
	if continuation.DestinationProvider != "codex" || continuation.Mode != state.ContinuationNativeImport {
		return fmt.Errorf("continuation mode %q is not valid for Codex", continuation.Mode)
	}
	r.continuation = &continuation
	if continuation.ProviderContext == "applying" {
		return errors.New("Codex continuation import has an unknown prior outcome; refusing to duplicate history")
	}
	if continuation.ProviderContext != "applied" {
		continuation.ProviderContext = "applying"
		if err := state.WriteContinuation(r.paths.Continuation, continuation); err != nil {
			return fmt.Errorf("mark Codex continuation applying: %w", err)
		}
		messages := make([]codexapp.ImportedMessage, 0, len(continuation.Messages))
		for _, message := range continuation.Messages {
			messages = append(messages, codexapp.ImportedMessage{Role: message.Role, Text: message.Text})
		}
		if err := r.client.InjectMessages(r.ctx, r.conversationID, messages); err != nil {
			continuation.ProviderContext = ""
			_ = state.WriteContinuation(r.paths.Continuation, continuation)
			return err
		}
		continuation.ProviderContext = "applied"
	}
	if !continuation.LocalHistoryReady {
		for _, message := range continuation.Messages {
			raw, encodeErr := codexapp.ImportedHistoryEvent(
				r.conversationID, message.Role, message.Text, continuation.SourceHistoryID,
				continuationTime(message.Timestamp),
			)
			if encodeErr == nil {
				r.appendStructured(raw)
			}
		}
		continuation.LocalHistoryReady = true
	}
	if err := state.WriteContinuation(r.paths.Continuation, continuation); err != nil {
		return fmt.Errorf("finish Codex continuation import: %w", err)
	}
	r.continuation = &continuation
	return nil
}

func codexConversationOptions(cfg config) codexapp.ConversationOptions {
	options := codexapp.ConversationOptions{
		CWD: cfg.cwd, Model: codexArgValue(cfg.args, "--model", "-m"),
		Effort:      codexConfigValue(cfg.args, "model_reasoning_effort"),
		ServiceTier: codexConfigValue(cfg.args, "service_tier"),
	}
	if codexHasArg(cfg.args, "--dangerously-bypass-approvals-and-sandbox") {
		options.ApprovalPolicy = codexapp.ApprovalNever
		options.Sandbox = codexapp.SandboxDangerFullAccess
		return options
	}
	options.ApprovalPolicy = codexArgValue(cfg.args, "--ask-for-approval", "-a")
	options.Sandbox = codexArgValue(cfg.args, "--sandbox", "-s")
	return options
}

func codexHasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func codexArgValue(args []string, names ...string) string {
	for index := 0; index+1 < len(args); index++ {
		for _, name := range names {
			if args[index] == name {
				return args[index+1]
			}
		}
	}
	return ""
}

func codexConfigValue(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-c" && args[index] != "--config" {
			continue
		}
		if value, ok := strings.CutPrefix(args[index+1], key+"="); ok {
			return strings.Trim(value, `"'`)
		}
	}
	return ""
}

func (r *codexAppRunner) writeMetadata() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeMetadataLocked()
}

func (r *codexAppRunner) writeMetadataLocked() error {
	return state.WriteMetadata(r.paths.Meta, state.Metadata{
		ID: r.cfg.id, Name: r.cfg.name, Description: r.cfg.description,
		DescriptionSource: r.cfg.descriptionSource, Kind: r.cfg.kind, SpecPath: r.cfg.specPath,
		Profile: r.cfg.profile, ConfigDir: r.cfg.configDir,
		Cmd: r.cfg.cmd, Args: r.cfg.args, Cwd: r.cfg.cwd,
		Cols: r.cfg.cols, Rows: r.cfg.rows, CreatedAt: r.createdAt,
		PID: os.Getpid(), SockPath: r.paths.Socket,
		ConversationID: r.conversationID, RemoteEndpoint: r.remoteEndpoint,
		ContinuedFromHistoryID: continuationHistoryID(r.continuation),
		ContinuedFromProvider:  continuationSourceProvider(r.continuation),
		ContinuationMode:       continuationMode(r.continuation),
		ImportedMessageCount:   continuationMessageCount(r.continuation),
	})
}

func (r *codexAppRunner) openHistory() error {
	file, err := os.OpenFile(r.paths.Structured, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := os.Chmod(r.paths.Structured, 0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), structuredScannerBuffer)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		r.history = append(r.history, append(json.RawMessage(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return err
	}
	r.historyFile = file
	return nil
}

func (r *codexAppRunner) closeHistory() {
	if r.historyFile != nil {
		_ = r.historyFile.Close()
	}
}

func (r *codexAppRunner) acceptLoop() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				r.logger.Printf("codex host socket accept failed: %v", err)
			}
			return
		}
		go r.serveClient(conn)
	}
}

func (r *codexAppRunner) serveClient(conn net.Conn) {
	c := &client{conn: conn}
	r.streamMu.Lock()
	r.mu.Lock()
	r.clients[c] = struct{}{}
	h := hello{
		ID: r.cfg.id, Cmd: r.cfg.cmd, Args: r.cfg.args, Cwd: r.cfg.cwd,
		Cols: r.cfg.cols, Rows: r.cfg.rows, CreatedAt: r.createdAt,
		PID: os.Getpid(), ProtocolVersion: proto.ProtocolVersion, RuntimeVersion: version,
		ConversationID: r.conversationID, RemoteEndpoint: r.remoteEndpoint,
	}
	r.mu.Unlock()
	payload, err := json.Marshal(h)
	if err == nil {
		err = c.write(proto.Hello, payload)
	}
	r.streamMu.Unlock()
	if err != nil {
		_ = conn.Close()
	}
	defer func() {
		_ = conn.Close()
		r.mu.Lock()
		delete(r.clients, c)
		r.mu.Unlock()
	}()
	for {
		frame, err := proto.Read(conn)
		if err != nil {
			return
		}
		if err := r.handleFrame(c, frame); err != nil {
			return
		}
	}
}

func (r *codexAppRunner) handleFrame(c *client, frame proto.Frame) error {
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

func (r *codexAppRunner) configureModel(control proto.ModelControl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active {
		return errors.New("Codex turn is active")
	}
	previous := append([]string(nil), r.cfg.args...)
	previousModel := codexArgValue(previous, "--model", "-m")
	previousEffort := codexConfigValue(previous, "model_reasoning_effort")
	if err := r.client.SetConversationModel(r.conversationID, control.Model, control.Effort); err != nil {
		return err
	}
	r.cfg.args = withArgumentValue(r.cfg.args, control.Model, "--model", "-m")
	r.cfg.args = withConfigValue(r.cfg.args, "model_reasoning_effort", control.Effort)
	if err := r.writeMetadataLocked(); err != nil {
		r.cfg.args = previous
		_ = r.client.SetConversationModel(r.conversationID, previousModel, previousEffort)
		return err
	}
	return nil
}

func (r *codexAppRunner) handleInput(data string) {
	if isCodexInterruptInput(data) {
		r.mu.Lock()
		active := r.active
		r.mu.Unlock()
		if active {
			go r.interruptTurn()
		}
		return
	}
	r.mu.Lock()
	parts := strings.Split(data, "\r")
	for index, part := range parts {
		r.composer.WriteString(part)
		if index == len(parts)-1 {
			continue
		}
		text := cleanComposerInput(r.composer.String())
		r.composer.Reset()
		if text == "" || r.active {
			continue
		}
		r.active = true
		go r.runTurn(text)
	}
	r.mu.Unlock()
}

func isCodexInterruptInput(data string) bool {
	return data == "\x1b" || data == "\x03"
}

func (r *codexAppRunner) interruptTurn() {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	if err := r.client.InterruptTurn(ctx, r.conversationID); err != nil {
		r.logger.Printf("interrupt Codex turn failed: %v", err)
	}
}

func cleanComposerInput(value string) string {
	value = strings.ReplaceAll(value, "\x1b[200~", "")
	value = strings.ReplaceAll(value, "\x1b[201~", "")
	return value
}

func (r *codexAppRunner) runTurn(text string) {
	defer func() {
		r.mu.Lock()
		r.active = false
		r.mu.Unlock()
	}()
	stream, err := r.client.SendUserTurn(r.ctx, r.conversationID, text)
	if err != nil {
		r.recordTurnFailure(err)
		return
	}
	user, err := codexapp.UserHistoryEvent(r.conversationID, text, time.Now())
	if err == nil {
		r.appendStructured(user)
	}
	for event := range stream.Events {
		raw, encodeErr := codexapp.HistoryEvent(event, time.Now())
		if encodeErr == nil {
			r.appendStructured(raw)
		}
	}
	if _, err := stream.Result(r.ctx); err != nil && !errors.Is(err, context.Canceled) {
		r.recordTurnFailure(err)
	}
}

func (r *codexAppRunner) recordTurnFailure(err error) {
	raw, encodeErr := codexapp.HistoryEvent(codexapp.TurnComplete{
		ConversationID: r.conversationID, Status: "failed",
		Error: &codexapp.TurnError{Message: err.Error()},
	}, time.Now())
	if encodeErr == nil {
		r.appendStructured(raw)
	}
}

func (r *codexAppRunner) appendStructured(raw json.RawMessage) {
	if len(raw)+1 > proto.MaxFrameLen {
		return
	}
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	encoded := append(append([]byte(nil), raw...), '\n')
	if _, err := r.historyFile.Write(encoded); err != nil {
		r.logger.Printf("append structured history failed: %v", err)
	}
	r.mu.Lock()
	r.history = append(r.history, append(json.RawMessage(nil), raw...))
	clients := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		if err := c.write(proto.Structured, raw); err != nil {
			_ = c.conn.Close()
		}
	}
}

func (r *codexAppRunner) snapshot() string {
	r.mu.Lock()
	history := cloneStructured(r.history)
	r.mu.Unlock()
	var output strings.Builder
	for _, raw := range history {
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		message, ok := event["message"].(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		text := codexMessageText(message["content"])
		if (role != "user" && role != "assistant") || strings.TrimSpace(text) == "" {
			continue
		}
		if output.Len() > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "[%s]\n%s", role, text)
	}
	return output.String()
}

func codexMessageText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var output strings.Builder
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok {
			output.WriteString(text)
		}
	}
	return output.String()
}

func cloneStructured(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = append(json.RawMessage(nil), values[index]...)
	}
	return result
}

func (r *codexAppRunner) shutdown(permanent bool, code int) {
	r.shutdownOnce.Do(func() {
		r.cancel()
		r.streamMu.Lock()
		if r.listener != nil {
			_ = r.listener.Close()
		}
		_ = ipc.Remove(r.paths.Socket)
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
			_ = c.conn.Close()
		}
		if r.client != nil {
			_ = r.client.Close()
		}
		r.closeHistory()
		if permanent {
			_ = os.Remove(r.paths.Meta)
			_ = os.Remove(r.paths.Structured)
		}
		r.streamMu.Unlock()
		r.done <- code
	})
}
