// Package codexapp drives Codex conversations through the structured
// app-server JSON-RPC protocol instead of scraping terminal output.
package codexapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultCodexCommand = "codex"
	clientName          = "sessions"
	clientVersion       = "1"

	ApprovalUntrusted = "untrusted"
	ApprovalOnRequest = "on-request"
	ApprovalNever     = "never"

	SandboxReadOnly         = "read-only"
	SandboxWorkspaceWrite   = "workspace-write"
	SandboxDangerFullAccess = "danger-full-access"
)

var ErrClosed = errors.New("codex app-server client closed")

// Options controls the proxy process. The zero value starts the managed
// daemon and connects through `codex app-server proxy`. If this Codex install
// cannot run the managed daemon, the client falls back to an owned Unix-socket
// app-server that remains attachable for the lifetime of the client.
type Options struct {
	CodexPath             string
	SocketPath            string
	SkipDaemonStart       bool
	ManagedDaemonRequired bool
}

// ConversationOptions controls a persistent, TUI-attachable conversation.
// Empty approval and sandbox values default to never and danger-full-access,
// matching Sessions' skip-permissions mode.
type ConversationOptions struct {
	CWD                   string
	Model                 string
	Effort                string
	ServiceTier           string
	ApprovalPolicy        string
	Sandbox               string
	DeveloperInstructions string
}

// Conversation is the durable app-server thread identity returned when an
// existing thread is resumed after a runner or daemon restart.
type Conversation struct {
	ID string
}

type threadResumeParams struct {
	ApprovalPolicy        string         `json:"approvalPolicy,omitempty"`
	Config                map[string]any `json:"config,omitempty"`
	CWD                   string         `json:"cwd,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions,omitempty"`
	Model                 string         `json:"model,omitempty"`
	Sandbox               string         `json:"sandbox,omitempty"`
	ServiceTier           string         `json:"serviceTier,omitempty"`
	ThreadID              string         `json:"threadId"`
}

type threadResumeResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type conversationDefaults struct {
	approvalPolicy string
	effort         string
	model          string
	serviceTier    string
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e rpcError) Error() string {
	if len(e.Data) == 0 {
		return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s (%s)", e.Code, e.Message, e.Data)
}

type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type callResponse struct {
	result json.RawMessage
	err    error
}

type processResult struct {
	err    error
	stderr string
}

// Client owns one long-lived stdio proxy connection. It is safe for
// concurrent request calls; each conversation permits one active turn.
type Client struct {
	transport messageTransport
	stop      func()
	process   <-chan processResult

	writeMu        sync.Mutex
	mu             sync.Mutex
	nextID         uint64
	pending        map[string]chan callResponse
	turns          map[string]*turnState
	convs          map[string]conversationDefaults
	remoteEndpoint string
	closed         bool
}

// NewClient starts (or reuses) the managed app-server daemon, launches the
// stdio proxy, and completes the required initialize handshake.
func NewClient(ctx context.Context, options Options) (*Client, error) {
	codexPath := options.CodexPath
	if codexPath == "" {
		codexPath = defaultCodexCommand
	}
	resolved, err := exec.LookPath(codexPath)
	if err != nil {
		return nil, fmt.Errorf("find codex executable: %w", err)
	}

	socketPath := options.SocketPath
	var localServer *localAppServer
	if !options.SkipDaemonStart {
		command := exec.CommandContext(ctx, resolved, "app-server", "daemon", "start")
		if output, err := command.CombinedOutput(); err != nil {
			daemonErr := fmt.Errorf("start codex app-server daemon: %w: %s", err, strings.TrimSpace(string(output)))
			if options.ManagedDaemonRequired {
				return nil, daemonErr
			}
			if useDirectAppServerFallback() {
				client, fallbackErr := startDirectAppServerClient(ctx, resolved)
				if fallbackErr != nil {
					return nil, fmt.Errorf("%v; direct fallback app-server failed: %w", daemonErr, fallbackErr)
				}
				return client, nil
			}
			localServer, err = startLocalAppServer(ctx, resolved)
			if err != nil {
				return nil, fmt.Errorf("%v; fallback app-server failed: %w", daemonErr, err)
			}
			socketPath = localServer.socketPath
		}
	}

	args := []string{"app-server", "proxy"}
	if socketPath != "" {
		args = append(args, "--sock", socketPath)
	}
	command := exec.Command(resolved, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		if localServer != nil {
			localServer.Close()
		}
		return nil, fmt.Errorf("open app-server proxy stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		if localServer != nil {
			localServer.Close()
		}
		return nil, fmt.Errorf("open app-server proxy stdout: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if localServer != nil {
			localServer.Close()
		}
		return nil, fmt.Errorf("start codex app-server proxy: %w", err)
	}
	process := make(chan processResult, 1)
	go func() {
		err := command.Wait()
		process <- processResult{err: err, stderr: strings.TrimSpace(stderr.String())}
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = stdin.Close()
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			if localServer != nil {
				localServer.Close()
			}
		})
	}
	transport, err := newProxyWebSocketTransport(ctx, stdin, stdout)
	if err != nil {
		stop()
		return nil, err
	}
	client, err := newClientWithTransport(ctx, transport, stop, process)
	if err != nil {
		stop()
		return nil, err
	}
	if socketPath == "" {
		client.remoteEndpoint = "unix://"
	} else {
		client.remoteEndpoint = "unix://" + socketPath
	}
	return client, nil
}

func startDirectAppServerClient(ctx context.Context, codexPath string) (*Client, error) {
	command := exec.Command(codexPath, "app-server", "--stdio")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open direct app-server stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open direct app-server stdout: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start direct app-server: %w", err)
	}
	process := make(chan processResult, 1)
	go func() {
		process <- processResult{err: command.Wait(), stderr: strings.TrimSpace(stderr.String())}
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = stdin.Close()
			if command.Process != nil {
				_ = command.Process.Kill()
			}
		})
	}
	client, err := newClient(ctx, stdin, stdout, stop, process)
	if err != nil {
		stop()
		return nil, err
	}
	// Direct stdio is deliberately an owned fallback rather than a public
	// attach endpoint. The durable Codex conversation can still be resumed.
	client.remoteEndpoint = ""
	return client, nil
}

type localAppServer struct {
	socketPath string
	directory  string
	command    *exec.Cmd
	done       <-chan error
	once       sync.Once
}

func startLocalAppServer(ctx context.Context, codexPath string) (*localAppServer, error) {
	directory, err := os.MkdirTemp("/tmp", "sessions-appserver-")
	if err != nil {
		return nil, fmt.Errorf("create fallback app-server directory: %w", err)
	}
	socketPath := filepath.Join(directory, "app-server.sock")
	command := exec.Command(codexPath, "app-server", "--listen", "unix://"+socketPath)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("start fallback app-server: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	server := &localAppServer{
		socketPath: socketPath,
		directory:  directory,
		command:    command,
		done:       done,
	}

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return server, nil
		}
		select {
		case err := <-done:
			_ = os.RemoveAll(directory)
			return nil, fmt.Errorf("fallback app-server exited: %w: %s", err, strings.TrimSpace(stderr.String()))
		case <-startupCtx.Done():
			server.Close()
			return nil, fmt.Errorf("wait for fallback app-server socket: %w", startupCtx.Err())
		case <-ticker.C:
		}
	}
}

func (s *localAppServer) Close() {
	s.once.Do(func() {
		if s.command.Process != nil {
			_ = s.command.Process.Kill()
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
		_ = os.RemoveAll(s.directory)
	})
}

func newClient(
	ctx context.Context,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
	stop func(),
	process <-chan processResult,
) (*Client, error) {
	return newClientWithTransport(ctx, newJSONLTransport(stdin, stdout), stop, process)
}

func newClientWithTransport(
	ctx context.Context,
	transport messageTransport,
	stop func(),
	process <-chan processResult,
) (*Client, error) {
	client := &Client{
		transport: transport,
		stop:      stop,
		process:   process,
		pending:   make(map[string]chan callResponse),
		turns:     make(map[string]*turnState),
		convs:     make(map[string]conversationDefaults),
	}
	go client.readLoop(process != nil)
	if process != nil {
		go client.watchProcess()
	}

	var initialized InitializeResponse
	if err := client.call(ctx, "initialize", InitializeParams{
		ClientInfo: ClientInfo{Name: clientName, Version: clientVersion},
		Capabilities: InitializeCapabilities{
			ExperimentalAPI: true,
		},
	}, &initialized); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := client.writeJSON(struct {
		Method string `json:"method"`
	}{Method: "initialized"}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("notify codex app-server initialized: %w", err)
	}
	return client, nil
}

// Close terminates only this client's proxy. The managed daemon remains
// available so a TUI can attach to conversations created by the client.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	if c.stop != nil {
		c.stop()
	}
	_ = c.transport.Close()
	return nil
}

// RemoteEndpoint returns the Unix endpoint a stock Codex TUI can attach to
// while this client is alive.
func (c *Client) RemoteEndpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remoteEndpoint
}

// NewConversation creates a persistent app-server thread and returns its ID.
func (c *Client) NewConversation(ctx context.Context, options ConversationOptions) (string, error) {
	if options.CWD == "" {
		return "", errors.New("conversation cwd is required")
	}
	cwd, err := filepath.Abs(options.CWD)
	if err != nil {
		return "", fmt.Errorf("resolve conversation cwd: %w", err)
	}
	approvalPolicy := options.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = ApprovalNever
	}
	if !validApprovalPolicy(approvalPolicy) {
		return "", fmt.Errorf("unsupported approval policy %q", approvalPolicy)
	}
	sandbox := options.Sandbox
	if sandbox == "" {
		sandbox = SandboxDangerFullAccess
	}
	if !validSandbox(sandbox) {
		return "", fmt.Errorf("unsupported sandbox %q", sandbox)
	}

	var response ThreadStartResponse
	err = c.call(ctx, "thread/start", struct {
		ThreadStartParams
		DeveloperInstructions string `json:"developerInstructions,omitempty"`
	}{
		ThreadStartParams: ThreadStartParams{
			ApprovalPolicy: approvalPolicy,
			Config:         effortConfig(options.Effort),
			CWD:            cwd,
			Model:          options.Model,
			Sandbox:        sandbox,
			ServiceTier:    options.ServiceTier,
		},
		DeveloperInstructions: options.DeveloperInstructions,
	}, &response)
	if err != nil {
		return "", fmt.Errorf("start Codex conversation: %w", err)
	}
	if response.Thread.ID == "" {
		return "", errors.New("start Codex conversation: empty thread id")
	}
	c.mu.Lock()
	c.convs[response.Thread.ID] = conversationDefaults{
		approvalPolicy: approvalPolicy,
		effort:         options.Effort,
		model:          options.Model,
		serviceTier:    options.ServiceTier,
	}
	c.mu.Unlock()
	return response.Thread.ID, nil
}

// ResumeConversation reconnects this client to a durable app-server thread.
// It also restores the per-turn defaults required by SendUserTurn.
func (c *Client) ResumeConversation(ctx context.Context, conversationID string, options ConversationOptions) (Conversation, error) {
	if conversationID == "" {
		return Conversation{}, errors.New("conversation id is required")
	}
	defaults, cwd, sandbox, err := validatedConversationOptions(options)
	if err != nil {
		return Conversation{}, err
	}
	var response threadResumeResponse
	if err := c.call(ctx, "thread/resume", threadResumeParams{
		ApprovalPolicy:        defaults.approvalPolicy,
		Config:                effortConfig(defaults.effort),
		CWD:                   cwd,
		Model:                 defaults.model,
		Sandbox:               sandbox,
		ServiceTier:           defaults.serviceTier,
		ThreadID:              conversationID,
		DeveloperInstructions: options.DeveloperInstructions,
	}, &response); err != nil {
		return Conversation{}, fmt.Errorf("resume Codex conversation: %w", err)
	}
	if response.Thread.ID == "" {
		return Conversation{}, errors.New("resume Codex conversation: empty thread id")
	}
	if response.Thread.ID != conversationID {
		return Conversation{}, fmt.Errorf("resume Codex conversation: got thread %q, want %q", response.Thread.ID, conversationID)
	}
	c.mu.Lock()
	c.convs[conversationID] = defaults
	c.mu.Unlock()
	return Conversation{ID: conversationID}, nil
}

// ImportedMessage is one authored message that may be materialized in a new
// Codex thread. It intentionally excludes tool output and provider-internal
// response items.
type ImportedMessage struct {
	Role string
	Text string
}

// InjectMessages uses Codex app-server's explicit history import endpoint.
// Callers must use it only for a newly-created thread: this method does not
// deduplicate messages and never mutates the source conversation.
func (c *Client) InjectMessages(ctx context.Context, conversationID string, messages []ImportedMessage) error {
	if conversationID == "" {
		return errors.New("conversation id is required")
	}
	if len(messages) == 0 {
		return errors.New("at least one imported message is required")
	}
	items := make([]map[string]any, 0, len(messages))
	for index, message := range messages {
		text := strings.TrimSpace(message.Text)
		if text == "" {
			return fmt.Errorf("imported message %d is empty", index)
		}
		switch message.Role {
		case "user":
			items = append(items, map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": text}},
			})
		case "assistant":
			items = append(items, map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text}},
			})
		default:
			return fmt.Errorf("imported message %d has unsupported role %q", index, message.Role)
		}
	}
	params := struct {
		ThreadID string           `json:"threadId"`
		Items    []map[string]any `json:"items"`
	}{ThreadID: conversationID, Items: items}
	if err := c.call(ctx, "thread/inject_items", params, nil); err != nil {
		return fmt.Errorf("inject Codex history: %w", err)
	}
	return nil
}

func validatedConversationOptions(options ConversationOptions) (conversationDefaults, string, string, error) {
	if options.CWD == "" {
		return conversationDefaults{}, "", "", errors.New("conversation cwd is required")
	}
	cwd, err := filepath.Abs(options.CWD)
	if err != nil {
		return conversationDefaults{}, "", "", fmt.Errorf("resolve conversation cwd: %w", err)
	}
	approvalPolicy := options.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = ApprovalNever
	}
	if !validApprovalPolicy(approvalPolicy) {
		return conversationDefaults{}, "", "", fmt.Errorf("unsupported approval policy %q", approvalPolicy)
	}
	sandbox := options.Sandbox
	if sandbox == "" {
		sandbox = SandboxDangerFullAccess
	}
	if !validSandbox(sandbox) {
		return conversationDefaults{}, "", "", fmt.Errorf("unsupported sandbox %q", sandbox)
	}
	return conversationDefaults{
		approvalPolicy: approvalPolicy,
		effort:         options.Effort,
		model:          options.Model,
		serviceTier:    options.ServiceTier,
	}, cwd, sandbox, nil
}

func effortConfig(effort string) map[string]any {
	if effort == "" {
		return nil
	}
	return map[string]any{"model_reasoning_effort": effort}
}

// SetConversationModel changes the defaults used by the next turn without
// rewriting durable provider history. The daemon validates the requested
// choice against the live model catalog before this reaches the runner.
func (c *Client) SetConversationModel(conversationID, model, effort string) error {
	if conversationID == "" {
		return errors.New("conversation id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	defaults, ok := c.convs[conversationID]
	if !ok {
		return fmt.Errorf("unknown conversation %q", conversationID)
	}
	defaults.model = model
	defaults.effort = effort
	c.convs[conversationID] = defaults
	return nil
}

// SendUserTurn starts a turn and returns its structured event stream. Result
// can be awaited independently while Events is drained concurrently or later.
func (c *Client) SendUserTurn(ctx context.Context, conversationID, text string) (*TurnStream, error) {
	if conversationID == "" {
		return nil, errors.New("conversation id is required")
	}
	if text == "" {
		return nil, errors.New("user turn text is required")
	}

	state := newTurnState(conversationID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		state.fail(ErrClosed)
		return nil, ErrClosed
	}
	defaults, ok := c.convs[conversationID]
	if !ok {
		c.mu.Unlock()
		state.fail(fmt.Errorf("unknown conversation %q", conversationID))
		return nil, fmt.Errorf("unknown conversation %q", conversationID)
	}
	if _, active := c.turns[conversationID]; active {
		c.mu.Unlock()
		state.fail(fmt.Errorf("conversation %q already has an active turn", conversationID))
		return nil, fmt.Errorf("conversation %q already has an active turn", conversationID)
	}
	c.turns[conversationID] = state
	c.mu.Unlock()

	// Queue the lifecycle edge before issuing turn/start. It is only exposed if
	// turn/start succeeds, but it remains ordered ahead of notifications that
	// can legally race the response.
	state.started("")
	var response TurnStartResponse
	err := c.call(ctx, "turn/start", TurnStartParams{
		ApprovalPolicy: defaults.approvalPolicy,
		Effort:         defaults.effort,
		Input:          []UserInput{{Type: "text", Text: text}},
		Model:          defaults.model,
		ServiceTier:    defaults.serviceTier,
		ThreadID:       conversationID,
	}, &response)
	if err != nil {
		c.removeTurn(conversationID, state)
		state.fail(err)
		return nil, fmt.Errorf("start Codex user turn: %w", err)
	}
	if response.Turn.ID == "" {
		c.removeTurn(conversationID, state)
		err := errors.New("start Codex user turn: empty turn id")
		state.fail(err)
		return nil, err
	}
	if !state.acceptTurnID(response.Turn.ID) {
		c.removeTurn(conversationID, state)
		err := fmt.Errorf("start Codex user turn: response turn id %q did not match streamed turn", response.Turn.ID)
		state.fail(err)
		return nil, err
	}
	return state.stream(), nil
}

// SteerTurn submits input to Codex's active turn. Codex owns the scheduling
// semantics: it applies the message after the next tool call. Sessions does
// not maintain a second prompt queue or retry against a later turn.
func (c *Client) SteerTurn(ctx context.Context, conversationID, text string) (string, error) {
	if conversationID == "" {
		return "", errors.New("conversation id is required")
	}
	if text == "" {
		return "", errors.New("steering text is required")
	}
	state, turnID, err := c.activeTurn(ctx, conversationID)
	if err != nil {
		return "", err
	}
	var response TurnSteerResponse
	if err := c.call(ctx, "turn/steer", TurnSteerParams{
		ThreadID:       conversationID,
		ExpectedTurnID: turnID,
		Input:          []UserInput{{Type: "text", Text: text}},
	}, &response); err != nil {
		return "", fmt.Errorf("steer Codex turn: %w", err)
	}
	if response.TurnID == "" {
		return "", errors.New("steer Codex turn: empty turn id")
	}
	if response.TurnID != turnID || state.currentTurnID() != turnID {
		return "", fmt.Errorf("steer Codex turn: response turn id %q did not match active turn %q", response.TurnID, turnID)
	}
	return turnID, nil
}

// InterruptTurn asks app-server to stop the active turn without terminating
// the durable conversation or its runner. This is the structured equivalent
// of Escape in the Codex TUI and is safe for the Sessions chat surface.
func (c *Client) InterruptTurn(ctx context.Context, conversationID string) error {
	if conversationID == "" {
		return errors.New("conversation id is required")
	}
	_, turnID, err := c.activeTurn(ctx, conversationID)
	if err != nil {
		return err
	}
	params := struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}{ThreadID: conversationID, TurnID: turnID}
	if err := c.call(ctx, "turn/interrupt", params, nil); err != nil {
		return fmt.Errorf("interrupt Codex turn: %w", err)
	}
	return nil
}

func (c *Client) activeTurn(ctx context.Context, conversationID string) (*turnState, string, error) {
	c.mu.Lock()
	state := c.turns[conversationID]
	c.mu.Unlock()
	if state == nil {
		return nil, "", fmt.Errorf("conversation %q has no active turn", conversationID)
	}
	turnID := ""
	for turnID == "" {
		turnID = state.currentTurnID()
		if turnID != "" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("wait for conversation %q active turn: %w", conversationID, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
		c.mu.Lock()
		stillActive := c.turns[conversationID] == state
		c.mu.Unlock()
		if !stillActive {
			return nil, "", fmt.Errorf("conversation %q has no active turn", conversationID)
		}
	}
	return state, turnID, nil
}
