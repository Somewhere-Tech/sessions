// Command runner is the long-lived per-session PTY owner used by sessionsd.
// Its environment variables, state files, and socket protocol intentionally
// match runtime/testdata/node-runtime/src/runner.ts so either implementation can be swapped alone.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	idleShutdown       = 30 * time.Second
	clientOutboxFrames = 16
	maximumTail        = 4 * 1024

	// laneInterruptedExitCode is the conventional 128+SIGTERM encoding. A lane
	// stopped by logout, reboot, or a launchd unload never reports success, so
	// no reader can mistake an unfinished command for a completed one.
	laneInterruptedExitCode = 143
)

// laneInterruptedNotice marks the terminal record of a lane that shutdown
// ended first. The manifest is what stops the command from running a second
// time at the next login, so the record has to say plainly that the work did
// not finish and that nothing re-ran it.
const laneInterruptedNotice = "\r\n[sessions: this lane was stopped by shutdown before its command finished. " +
	"It was not re-run at the next login; start it again if the work still needs to happen.]\r\n"

var version = "0.2.19"

type config struct {
	id                string
	name              string
	description       string
	descriptionSource string
	kind              string
	specPath          string
	profile           string
	configDir         string
	stateDir          string
	cmd               string
	args              []string
	cwd               string
	cols              int
	rows              int
}

type hello struct {
	ID              string   `json:"id"`
	Cmd             string   `json:"cmd"`
	Args            []string `json:"args"`
	Cwd             string   `json:"cwd"`
	Cols            int      `json:"cols"`
	Rows            int      `json:"rows"`
	CreatedAt       int64    `json:"createdAt"`
	PID             int      `json:"pid"`
	CurrentSeq      uint32   `json:"currentSeq"`
	ProtocolVersion int      `json:"protocolVersion"`
	RuntimeVersion  string   `json:"runtimeVersion,omitempty"`
	ConversationID  string   `json:"conversationId,omitempty"`
	RemoteEndpoint  string   `json:"remoteEndpoint,omitempty"`
	ClaudeSessionID string   `json:"claudeSessionId,omitempty"`
}

type exitInfo struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
	Seq    uint32  `json:"seq"`
}

type resizeRequest struct {
	Cols float64 `json:"cols"`
	Rows float64 `json:"rows"`
}

type client struct {
	conn      net.Conn
	outbox    chan clientWrite
	closed    chan struct{}
	closeOnce sync.Once
}

type clientWrite struct {
	frame []byte
	done  chan error
}

func newClient(conn net.Conn) *client {
	c := &client{
		conn:   conn,
		outbox: make(chan clientWrite, clientOutboxFrames),
		closed: make(chan struct{}),
	}
	go c.writeLoop()
	return c
}

func (c *client) writeLoop() {
	for {
		select {
		case <-c.closed:
			return
		case request := <-c.outbox:
			err := c.writeFrame(request.frame)
			if request.done != nil {
				request.done <- err
			}
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *client) writeFrame(frame []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writeBytes(c.conn, frame)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *client) write(typ proto.Type, payload []byte) error {
	frame, err := proto.Encode(typ, payload)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	select {
	case c.outbox <- clientWrite{frame: frame, done: done}:
	case <-c.closed:
		return net.ErrClosed
	}
	select {
	case err := <-done:
		return err
	case <-c.closed:
		return net.ErrClosed
	}
}

func (c *client) enqueue(typ proto.Type, payload []byte) bool {
	frame, err := proto.Encode(typ, payload)
	if err != nil {
		return false
	}
	return c.enqueueFrame(frame)
}

func (c *client) enqueueFrame(frame []byte) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.outbox <- clientWrite{frame: frame}:
		return true
	default:
		// A viewer that cannot keep up must not stall the provider reader or
		// every other viewer. Its daemon can reconnect and replay from the
		// durable runner history.
		c.close()
		return false
	}
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *client) writeOutput(ev state.Event) error {
	b, err := proto.EncodeOutput(ev.Seq, ev.Data)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	select {
	case c.outbox <- clientWrite{frame: b, done: done}:
	case <-c.closed:
		return net.ErrClosed
	}
	select {
	case err := <-done:
		return err
	case <-c.closed:
		return net.ErrClosed
	}
}

type terminalHistory interface {
	Append(uint32, []byte) error
	Sync() error
	Close() error
	Unlink() error
}

type runner struct {
	cfg        config
	paths      state.Paths
	createdAt  int64
	process    childProcess
	listener   net.Listener
	log        *state.EventLog
	persistent terminalHistory
	logger     *log.Logger

	// streamMu makes REPLAY output atomic relative to live output, matching
	// JavaScript's single event loop. It also guarantees HELLO is first.
	streamMu sync.Mutex
	mu       sync.Mutex
	clients  map[*client]struct{}
	cols     int
	rows     int
	exited   bool
	exit     *exitInfo
	idle     *time.Timer

	shutdownOnce sync.Once
	manifestOnce sync.Once
	readDone     chan struct{}
	jsonlMissing bool
	gitBaseline  gitWorktreeState
}

func main() {
	os.Exit(run())
}

func run() int {
	cfg, malformedArgs, err := configFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		return 2
	}
	// TypeScript intentionally exits successfully on corrupt args so launchd's
	// SuccessfulExit=false policy does not create a crash loop.
	if malformedArgs != "" {
		fmt.Fprintf(os.Stderr, "runner: failed to parse RUNNER_ARGS_JSON=%q: %s\n", malformedArgs, errText(malformedArgs))
		return 0
	}
	if err := state.EnsureDir(cfg.stateDir); err != nil {
		fmt.Fprintln(os.Stderr, "runner: create state directory:", err)
		return 1
	}
	paths := state.For(cfg.stateDir, cfg.id)
	if anotherRunnerAlive(paths.Socket, cfg.id) {
		fmt.Fprintf(os.Stderr, "runner %s: another instance already owns %s — exiting to avoid a duplicate\n", cfg.id, paths.Socket)
		return 0
	}
	_ = ipc.Remove(paths.Socket)

	logFile, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner: open log:", err)
		return 1
	}
	defer logFile.Close()
	_ = os.Chmod(paths.Log, 0o644)
	logger := log.New(io.MultiWriter(os.Stderr, logFile), "runner: ", log.LstdFlags|log.Lmicroseconds)
	if cfg.kind == state.KindCodexAppServer {
		return runCodexAppServer(cfg, paths, logger)
	}
	if cfg.kind == state.KindClaudeStructured {
		return runClaudeStructured(cfg, paths, logger)
	}
	if code, guarded := guardCompletedLane(cfg, paths, logger); guarded {
		return code
	}

	spawnArgs, jsonlMissing := respawnArgs(cfg, paths.Events)
	command := exec.Command(cfg.cmd, spawnArgs...)
	command.Dir = cfg.cwd
	command.Env = childEnv()
	var gitBaseline gitWorktreeState
	if cfg.kind == state.KindLane {
		gitBaseline = captureGitWorktreeState(cfg.cwd)
	}
	process, err := startChildProcess(command, cfg.cols, cfg.rows, cfg.kind == state.KindLane)
	if err != nil {
		logger.Printf("spawn %q failed: %v", cfg.cmd, err)
		return 1
	}

	persistent, err := state.OpenPersistent(paths.Events)
	if err != nil {
		logger.Printf("open persistent log failed: %v", err)
		_ = process.RequestStop()
		_ = process.CloseOutput()
		_ = process.Wait(cfg.kind == state.KindLane)
		return 1
	}
	eventLog := state.NewEventLog(state.DefaultEventCap)
	restored, err := state.Restore(paths.Events)
	if err != nil {
		logger.Printf("restore persistent log failed: %v", err)
	}
	for _, ev := range restored {
		eventLog.PushAt(ev.Seq, ev.Data)
	}
	if len(restored) > 0 {
		banner := fmt.Sprintf("\r\n\x1b[2m[sessions: replayed %d events from disk · %s]\x1b[0m\r\n", len(restored), jsISOString(time.Now()))
		eventLog.Push([]byte(banner))
	}
	if jsonlMissing {
		notice := "\r\n\x1b[33m[sessions: backing Claude JSONL not found — attempted --resume may fail; events history is preserved]\x1b[0m\r\n"
		eventLog.Push([]byte(notice))
	}

	r := &runner{
		cfg:          cfg,
		paths:        paths,
		createdAt:    time.Now().UnixMilli(),
		process:      process,
		log:          eventLog,
		persistent:   persistent,
		logger:       logger,
		clients:      make(map[*client]struct{}),
		cols:         cfg.cols,
		rows:         cfg.rows,
		readDone:     make(chan struct{}),
		jsonlMissing: jsonlMissing,
		gitBaseline:  gitBaseline,
	}
	meta := state.Metadata{
		ID: cfg.id, Name: cfg.name, Description: cfg.description,
		DescriptionSource: cfg.descriptionSource, Kind: cfg.kind, SpecPath: cfg.specPath,
		Profile: cfg.profile, ConfigDir: cfg.configDir,
		Cmd: cfg.cmd, Args: cfg.args, Cwd: cfg.cwd,
		Cols: cfg.cols, Rows: cfg.rows, CreatedAt: r.createdAt,
		PID: process.PID(), SockPath: paths.Socket,
	}
	meta.ConversationID, meta.ClaudeSessionID = providerConversationIdentity(cfg)
	// A PTY runner rebuilds metadata from its launch config too, so a launchd
	// restart of a tagged or set-aside session must not drop those fields.
	if err := state.WriteRunnerMetadata(paths.Meta, meta); err != nil {
		logger.Printf("write metadata failed: %v", err)
		r.shutdown(false, 1)
	}

	listener, err := ipc.Listen(paths.Socket)
	if err != nil {
		logger.Printf("socket listen failed: %v", err)
		r.shutdown(false, 1)
	}
	r.listener = listener

	go r.readOutput()
	go r.waitChild()
	go r.acceptLoop()

	// runner.ts ignores SIGINT/SIGHUP because daemon/editor restarts must not
	// tear down sessions. SIGTERM is the launchd/reboot preservation path.
	signal.Ignore(os.Interrupt, syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		r.mu.Lock()
		exited := r.exited
		permanent := exited && !r.jsonlMissing
		r.mu.Unlock()
		if !exited {
			// Shutdown is reaching this lane before its command finished.
			// waitChild will never run, so this is the only chance to leave a
			// terminal record — and without one, RunAtLoad starts the command
			// again at the next login.
			r.writeLaneManifest(r.interruptedManifest)
		}
		r.shutdown(permanent, 0)
	}()

	select {}
}

// guardCompletedLane keeps a lane's command from running twice. The runner's
// launchd job carries RunAtLoad, so a lane interrupted by a reboot is started
// again at the next login; without this check a deploy, migration, or any
// other one-shot command would silently repeat. A completion manifest is the
// durable proof that this lane already reached a terminal record, so the
// runner leaves that record alone and exits successfully.
//
// This does not block the KeepAlive{SuccessfulExit:false} restart of a crashed
// runner: a runner that dies before its lane finishes never wrote a manifest,
// so the guard does not fire. It fires only after the terminal record exists,
// where re-running the command would be the wrong answer anyway.
func guardCompletedLane(cfg config, paths state.Paths, logger *log.Logger) (int, bool) {
	if cfg.kind != state.KindLane {
		return 0, false
	}
	_, err := os.Stat(paths.Manifest)
	if err == nil {
		logger.Printf(
			"lane %s already finished and recorded %s; not running %q again — read the record, or create a new lane to run the command again",
			cfg.id, paths.Manifest, cfg.cmd,
		)
		return 0, true
	}
	if errors.Is(err, os.ErrNotExist) {
		return 0, false
	}
	// An unreadable record cannot prove the command has not already run, and
	// re-running a user's command is the one outcome that cannot be undone.
	logger.Printf(
		"read lane completion record %s failed: %v; not running %q because a repeat run cannot be taken back — fix or remove the record, then create a new lane",
		paths.Manifest, err, cfg.cmd,
	)
	return 0, true
}

func configFromEnv() (config, string, error) {
	id := os.Getenv("RUNNER_ID")
	if id == "" {
		return config{}, "", errors.New("RUNNER_ID env var required")
	}
	name := strings.TrimSpace(os.Getenv("RUNNER_NAME"))
	description := strings.TrimSpace(os.Getenv("RUNNER_DESCRIPTION"))
	descriptionSource := strings.TrimSpace(os.Getenv("RUNNER_DESCRIPTION_SOURCE"))
	kind := strings.TrimSpace(os.Getenv("RUNNER_KIND"))
	if kind != "" && kind != state.KindLane && kind != state.KindCodexAppServer && kind != state.KindClaudeStructured {
		return config{}, "", fmt.Errorf("unsupported RUNNER_KIND=%q", kind)
	}
	specPath := strings.TrimSpace(os.Getenv("RUNNER_SPEC_PATH"))
	profile := strings.TrimSpace(os.Getenv("RUNNER_PROFILE"))
	configDir := strings.TrimSpace(os.Getenv("RUNNER_CONFIG_DIR"))
	stateDir := os.Getenv("RUNNER_STATE_DIR")
	if stateDir == "" {
		var err error
		stateDir, err = state.DefaultRunnerDir()
		if err != nil {
			return config{}, "", err
		}
	}
	cmd := os.Getenv("RUNNER_CMD")
	if cmd == "" {
		cmd = defaultRunnerCommand()
	}
	rawArgs, present := os.LookupEnv("RUNNER_ARGS_JSON")
	args := []string{}
	if present && rawArgs != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return config{}, rawArgs, nil
		}
	}
	cwd := os.Getenv("RUNNER_CWD")
	if cwd == "" {
		var err error
		cwd, err = os.UserHomeDir()
		if err != nil {
			return config{}, "", err
		}
	}
	cols, err := envInt("RUNNER_COLS", 300)
	if err != nil {
		return config{}, "", err
	}
	rows, err := envInt("RUNNER_ROWS", 50)
	if err != nil {
		return config{}, "", err
	}
	if cols <= 0 || cols > 65535 || rows <= 0 || rows > 65535 {
		return config{}, "", fmt.Errorf("invalid PTY size %dx%d", cols, rows)
	}
	return config{
		id: id, name: name, description: description, descriptionSource: descriptionSource,
		kind: kind, specPath: specPath, profile: profile, configDir: configDir,
		stateDir: stateDir, cmd: cmd, args: args, cwd: cwd, cols: cols, rows: rows,
	}, "", nil
}

func envInt(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", name, raw, err)
	}
	return n, nil
}

func errText(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return err.Error()
	}
	return "expected a JSON string array"
}

func childEnv() []string {
	control := map[string]struct{}{
		"RUNNER_ID": {}, "RUNNER_CMD": {}, "RUNNER_ARGS_JSON": {},
		"RUNNER_CWD": {}, "RUNNER_COLS": {}, "RUNNER_ROWS": {},
		"RUNNER_STATE_DIR": {}, "RUNNER_NAME": {}, "RUNNER_KIND": {},
		"RUNNER_SPEC_PATH": {}, "RUNNER_DESCRIPTION": {}, "RUNNER_DESCRIPTION_SOURCE": {},
		"RUNNER_PROFILE": {}, "RUNNER_CONFIG_DIR": {},
		"TERM": {}, "COLORTERM": {},
		"SESSIONS_CODEX_APP_SERVER_SOCKET": {},
	}
	out := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, drop := control[key]; !drop {
			out = append(out, entry)
		}
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}

func anotherRunnerAlive(socketPath, expectedID string) bool {
	conn, err := ipc.DialTimeout(socketPath, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := proto.Read(conn)
	if err != nil || frame.Type != proto.Hello {
		return false
	}
	var greeting hello
	return json.Unmarshal(frame.Payload, &greeting) == nil && greeting.ID == expectedID
}

func respawnArgs(cfg config, eventsPath string) ([]string, bool) {
	st, err := os.Stat(eventsPath)
	if err != nil || st.Size() <= 0 {
		return cfg.args, false
	}
	// Both spellings have to be handled: leaving `--session-id=<uuid>` in place
	// respawns Claude against an id it already used, which it refuses, so the
	// conversation would be lost rather than resumed.
	args := append([]string(nil), cfg.args...)
	for i, arg := range args {
		if arg == providerargs.ClaudeSessionIDFlag {
			args[i] = "--resume"
			if i+1 >= len(args) || args[i+1] == "" {
				return args, false
			}
			return args, !claudeJSONLExists(args[i+1])
		}
		if id, ok := strings.CutPrefix(arg, providerargs.ClaudeSessionIDFlag+"="); ok {
			if id == "" {
				return cfg.args, false
			}
			args[i] = "--resume=" + id
			return args, !claudeJSONLExists(id)
		}
	}
	return cfg.args, false
}

func claudeJSONLExists(id string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	projects := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(projects, entry.Name(), id+".jsonl")); err == nil {
			return true
		}
	}
	return false
}

func (r *runner) acceptLoop() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				r.logger.Printf("socket accept failed: %v", err)
			}
			return
		}
		go r.serveClient(conn)
	}
}

func (r *runner) serveClient(conn net.Conn) {
	c := newClient(conn)
	// Prevent live OUTPUT from racing ahead of the greeting.
	r.streamMu.Lock()
	r.mu.Lock()
	r.clients[c] = struct{}{}
	// The post-exit grace period exists so a reconnecting daemon can still
	// replay. Disarm it here or the timer deletes the durable event file part
	// way through that replay; removeClient re-arms it on disconnect.
	r.cancelIdleShutdownLocked()
	h := r.helloLocked()
	exit := r.exit
	r.mu.Unlock()
	hPayload, err := json.Marshal(h)
	if err == nil {
		err = c.write(proto.Hello, hPayload)
	}
	if err == nil && exit != nil {
		var payload []byte
		payload, err = json.Marshal(exit)
		if err == nil {
			err = c.write(proto.Exit, payload)
		}
	}
	r.streamMu.Unlock()
	if err != nil {
		c.close()
	}

	defer func() {
		c.close()
		r.removeClient(c)
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

func (r *runner) helloLocked() hello {
	conversationID, claudeSessionID := providerConversationIdentity(r.cfg)
	return hello{
		ID: r.cfg.id, Cmd: r.cfg.cmd, Args: r.cfg.args, Cwd: r.cfg.cwd,
		Cols: r.cols, Rows: r.rows, CreatedAt: r.createdAt,
		PID: r.process.PID(), CurrentSeq: r.log.CurrentSeq(),
		ProtocolVersion: proto.ProtocolVersion, RuntimeVersion: version,
		ConversationID: conversationID, ClaudeSessionID: claudeSessionID,
	}
}

func providerConversationIdentity(cfg config) (conversationID, claudeSessionID string) {
	switch state.CommandTool(cfg.cmd) {
	case state.ToolClaude:
		providerID := providerargs.ClaudeSessionID(cfg.args)
		if providerargs.IsConversationUUID(providerID) {
			return providerID, providerID
		}
	case state.ToolCodex:
		providerID := providerargs.CodexConversationID(cfg.args)
		if providerargs.IsConversationUUID(providerID) {
			return providerID, ""
		}
	}
	return "", ""
}

func (r *runner) handleFrame(c *client, frame proto.Frame) error {
	switch frame.Type {
	case proto.Input:
		r.mu.Lock()
		exited := r.exited
		r.mu.Unlock()
		if !exited && r.process != nil {
			_, _ = r.process.Write(frame.Payload)
		}
	case proto.Resize:
		var request resizeRequest
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			return nil
		}
		cols, rows := int(request.Cols), int(request.Rows)
		if request.Cols != float64(cols) || request.Rows != float64(rows) || cols <= 0 || rows <= 0 || cols > 65535 || rows > 65535 {
			return nil
		}
		r.mu.Lock()
		exited := r.exited
		if !exited && r.process != nil {
			r.cols, r.rows = cols, rows
		}
		r.mu.Unlock()
		if !exited && r.process != nil {
			_ = r.process.Resize(cols, rows)
		}
	case proto.SnapshotReq:
		r.streamMu.Lock()
		defer r.streamMu.Unlock()
		snap := r.snapshotLocked()
		if len(snap)+1 > proto.MaxFrameLen {
			snap = snap[len(snap)-(proto.MaxFrameLen-1):]
		}
		return c.write(proto.SnapshotRes, snap)
	case proto.ReplayReq:
		if len(frame.Payload) < 4 {
			return nil
		}
		after := binary.BigEndian.Uint32(frame.Payload[:4])
		r.streamMu.Lock()
		defer r.streamMu.Unlock()
		replay := r.log.Since(after)
		for _, ev := range replay.Events {
			if err := c.writeOutput(ev); err != nil {
				return err
			}
		}
		return c.write(proto.ReplayDone, nil)
	case proto.Kill:
		r.mu.Lock()
		exited := r.exited
		r.mu.Unlock()
		if !exited {
			if err := r.process.RequestStop(); err != nil {
				_ = r.process.ForceKill()
			}
		}
	}
	return nil
}

func (r *runner) readOutput() {
	defer close(r.readDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.process.Read(buf)
		if n > 0 {
			r.recordOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (r *runner) recordOutput(data []byte) {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	ev := r.log.Push(data)
	if err := r.persistent.Append(ev.Seq, ev.Data); err != nil {
		r.logger.Printf("persistent.append failed: %v", err)
	}
	frame, err := proto.EncodeOutput(ev.Seq, ev.Data)
	if err != nil {
		r.logger.Printf("encode OUTPUT failed: %v", err)
		return
	}
	r.broadcastBytes(frame)
}

func (r *runner) waitChild() {
	info := r.process.Wait(r.cfg.kind == state.KindLane)
	select {
	case <-r.readDone:
	case <-time.After(250 * time.Millisecond):
		_ = r.process.CloseOutput()
		<-r.readDone
	}
	// The process may write a final line while handling Ctrl+C or immediately
	// before a forced Windows Job termination. readDone proves that output has
	// reached recordOutput; Sync makes its exact last sequence durable before
	// EXIT advertises that sequence to the daemon.
	if err := r.persistent.Sync(); err != nil {
		r.logger.Printf("persistent.sync before exit failed: %v", err)
	}
	r.writeLaneManifest(func() state.CompletionManifest { return r.completionManifest(info) })
	r.streamMu.Lock()
	info.Seq = r.log.CurrentSeq()
	r.mu.Lock()
	r.exited = true
	if !r.jsonlMissing {
		r.exit = &info
	} else {
		// The EXIT frame is still sent; only persistence cleanup differs.
		r.exit = &info
	}
	noClients := len(r.clients) == 0
	r.mu.Unlock()
	payload, marshalErr := json.Marshal(info)
	if marshalErr == nil {
		frame := proto.MustEncode(proto.Exit, payload)
		r.broadcastBytes(frame)
	}
	r.streamMu.Unlock()
	if noClients {
		r.scheduleIdleShutdown()
	}
}

// writeLaneManifest publishes a lane's terminal record exactly once. Shutdown
// can reach the SIGTERM path while waitChild is still draining the child, so
// the first durable record wins and the loser never overwrites it with a less
// informed account of the same lane.
func (r *runner) writeLaneManifest(build func() state.CompletionManifest) {
	if r.cfg.kind != state.KindLane {
		return
	}
	r.manifestOnce.Do(func() {
		if err := state.WriteCompletionManifest(r.paths.Manifest, build()); err != nil {
			r.logger.Printf(
				"write lane completion record %s failed: %v; this lane may run its command again at the next login — check what the command already did before starting it again",
				r.paths.Manifest, err,
			)
		}
	})
}

func (r *runner) completionManifest(info exitInfo) state.CompletionManifest {
	code := 0
	if info.Code != nil {
		code = *info.Code
	}
	return state.CompletionManifest{
		ExitCode: code, Signal: info.Signal, DurationMS: r.laneDuration(),
		LastOutputTail: r.manifestTail(), SpecPath: r.cfg.specPath,
		FilesChanged: gitFilesChangedSince(r.cfg.cwd, r.gitBaseline),
	}
}

// interruptedManifest is the terminal record for a lane that logout, reboot,
// or a launchd unload ended before its command finished. It deliberately skips
// the Git comparison that completionManifest performs: shutdown is time-boxed,
// and a second worktree scan can outlast the grace period, which would leave
// the lane with no record at all and re-run the command at the next login.
func (r *runner) interruptedManifest() state.CompletionManifest {
	signal := "SIGTERM"
	return state.CompletionManifest{
		ExitCode: laneInterruptedExitCode, Signal: &signal, DurationMS: r.laneDuration(),
		LastOutputTail: r.manifestTail() + laneInterruptedNotice,
		SpecPath:       r.cfg.specPath,
	}
}

func (r *runner) manifestTail() string {
	tail := r.snapshotLocked()
	if len(tail) > maximumTail {
		tail = tail[len(tail)-maximumTail:]
	}
	return string(tail)
}

func (r *runner) laneDuration() int64 {
	duration := time.Since(time.UnixMilli(r.createdAt)).Milliseconds()
	if duration < 0 {
		return 0
	}
	return duration
}

type gitWorktreeState struct {
	root  string
	scope string
	head  string
	paths map[string]string
}

// captureGitWorktreeState records only paths already visible to Git. Comparing
// this state at lane exit means files_changed describes what changed during
// this run, rather than the unrelated dirty-file count the repository happened
// to have before the lane started.
func captureGitWorktreeState(cwd string) gitWorktreeState {
	check := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	if output, err := check.Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
		return gitWorktreeState{}
	}
	rootCommand := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	rootOutput, err := rootCommand.Output()
	if err != nil {
		return gitWorktreeState{}
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" {
		return gitWorktreeState{}
	}
	scope, ok := gitWorkspaceScope(cwd, root)
	if !ok {
		return gitWorktreeState{}
	}
	head := ""
	if headOutput, headErr := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD").Output(); headErr == nil {
		head = strings.TrimSpace(string(headOutput))
	}
	if head == "" {
		emptyTree := exec.Command("git", "-C", root, "hash-object", "-t", "tree", "--stdin")
		emptyTree.Stdin = strings.NewReader("")
		if emptyTreeOutput, emptyTreeErr := emptyTree.Output(); emptyTreeErr == nil {
			head = strings.TrimSpace(string(emptyTreeOutput))
		}
	}
	status := exec.Command(
		"git", "-C", root, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--", scope,
	)
	output, err := status.Output()
	if err != nil {
		return gitWorktreeState{}
	}
	result := gitWorktreeState{root: root, scope: scope, head: head, paths: make(map[string]string)}
	skipRenameSource := false
	for _, encoded := range bytes.Split(output, []byte{0}) {
		if len(encoded) == 0 {
			continue
		}
		if skipRenameSource {
			skipRenameSource = false
			continue
		}
		record := string(encoded)
		path := ""
		switch {
		case strings.HasPrefix(record, "? ") || strings.HasPrefix(record, "! "):
			path = record[2:]
		case strings.HasPrefix(record, "1 "):
			path = porcelainPath(record, 9)
		case strings.HasPrefix(record, "2 "):
			path = porcelainPath(record, 10)
			skipRenameSource = true
		case strings.HasPrefix(record, "u "):
			path = porcelainPath(record, 11)
		}
		if path == "" {
			continue
		}
		result.paths[path] = record + "\x00" + worktreePathSignature(filepath.Join(root, filepath.FromSlash(path)))
	}
	return result
}

// gitWorkspaceScope keeps automatic files_changed accounting inside the
// directory the user selected. A Git repository rooted at the home directory
// must never turn a small lane into a recursive scan of Desktop, Documents,
// cloud drives, media libraries, or other protected siblings.
func gitWorkspaceScope(cwd, root string) (string, bool) {
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absCWD); resolveErr == nil {
		absCWD = resolved
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		absRoot = resolved
	}
	relative, err := filepath.Rel(absRoot, absCWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if relative == "." {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			absHome, absErr := filepath.Abs(home)
			if resolved, resolveErr := filepath.EvalSymlinks(absHome); resolveErr == nil {
				absHome = resolved
			}
			if absErr == nil && absHome == absRoot {
				return "", false
			}
		}
		if filepath.Dir(absRoot) == absRoot {
			return "", false
		}
		return ".", true
	}
	return filepath.ToSlash(relative), true
}

func porcelainPath(record string, fields int) string {
	parts := strings.SplitN(record, " ", fields)
	if len(parts) != fields {
		return ""
	}
	return parts[fields-1]
}

func worktreePathSignature(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return "missing:" + err.Error()
	}
	signature := fmt.Sprintf("%s:%d:%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		return signature + ":symlink:" + target + ":" + fmt.Sprint(readErr)
	}
	const maximumHashedFile = 64 * 1024 * 1024
	if !info.Mode().IsRegular() || info.Size() > maximumHashedFile {
		return signature
	}
	file, err := os.Open(path)
	if err != nil {
		return signature + ":open:" + err.Error()
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return signature + ":read:" + err.Error()
	}
	return signature + ":sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func gitFilesChangedSince(cwd string, before gitWorktreeState) *int {
	if before.root == "" {
		return nil
	}
	after := captureGitWorktreeState(cwd)
	if after.root == "" || after.root != before.root || after.scope != before.scope {
		return nil
	}
	paths := make(map[string]struct{}, len(before.paths)+len(after.paths))
	for path := range before.paths {
		paths[path] = struct{}{}
	}
	for path := range after.paths {
		paths[path] = struct{}{}
	}
	if before.head != "" && after.head != "" && before.head != after.head {
		command := exec.Command(
			"git", "-C", before.root, "diff", "--name-only", "-z", before.head, after.head, "--", before.scope,
		)
		output, err := command.Output()
		if err != nil {
			return nil
		}
		for _, path := range bytes.Split(output, []byte{0}) {
			if len(path) > 0 {
				paths[string(path)] = struct{}{}
			}
		}
	}
	changed := 0
	for path := range paths {
		if before.paths[path] != after.paths[path] {
			changed++
			continue
		}
		if before.head != after.head {
			command := exec.Command(
				"git", "-C", before.root, "diff", "--quiet", before.head, after.head, "--", path,
			)
			if err := command.Run(); err != nil {
				var exitError *exec.ExitError
				if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
					changed++
					continue
				}
				return nil
			}
		}
	}
	return &changed
}

func (r *runner) snapshotLocked() []byte {
	replay := r.log.Since(0)
	var out bytes.Buffer
	for _, ev := range replay.Events {
		_, _ = out.Write(ev.Data)
	}
	return out.Bytes()
}

func (r *runner) broadcastBytes(frame []byte) {
	r.mu.Lock()
	clients := make([]*client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.enqueueFrame(frame)
	}
}

func (r *runner) removeClient(c *client) {
	r.mu.Lock()
	delete(r.clients, c)
	shouldSchedule := r.exited && len(r.clients) == 0
	r.mu.Unlock()
	if shouldSchedule {
		r.scheduleIdleShutdown()
	}
}

func (r *runner) scheduleIdleShutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.exited || len(r.clients) > 0 || r.idle != nil {
		return
	}
	r.idle = time.AfterFunc(idleShutdown, func() {
		r.mu.Lock()
		r.idle = nil
		reconnected := !r.exited || len(r.clients) > 0
		permanent := !r.jsonlMissing
		r.mu.Unlock()
		if reconnected {
			// A client connected while this callback was already scheduled.
			// Stop cannot recall a fired timer, so the decision is re-checked
			// here; the next disconnect schedules a fresh grace period.
			return
		}
		r.shutdown(permanent, 0)
	})
}

// cancelIdleShutdownLocked disarms the post-exit grace period. The caller
// holds r.mu; Stop never waits for a running callback, so this cannot deadlock
// against a callback that is itself trying to take r.mu.
func (r *runner) cancelIdleShutdownLocked() {
	if r.idle == nil {
		return
	}
	r.idle.Stop()
	r.idle = nil
}

func (r *runner) shutdown(permanent bool, code int) {
	r.shutdownOnce.Do(func() {
		r.streamMu.Lock()
		if r.listener != nil {
			_ = r.listener.Close()
		}
		_ = ipc.Remove(r.paths.Socket)
		_ = os.Remove(r.paths.Meta)
		if permanent {
			_ = r.persistent.Unlink()
		} else {
			_ = r.persistent.Close()
		}
		if r.process != nil {
			_ = r.process.RequestStop()
			_ = r.process.CloseOutput()
		}
		r.streamMu.Unlock()
		os.Exit(code)
	})
}

func writeBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func jsISOString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
