package proto

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
)

const helloTimeout = 2 * time.Second

// SocketRunner is a daemon-side client for a real runner Unix socket.
type SocketRunner struct {
	conn net.Conn

	readOnce sync.Once
	writeMu  sync.Mutex
	replayMu sync.Mutex
	modelMu  sync.Mutex
	retryMu  sync.Mutex
	mu       sync.Mutex
	info     RunnerInfo
	exited   bool
	closed   bool
	subs     map[uint64]chan Event
	nextSub  uint64
	replay   *replayRequest
	model    *modelRequest
	retry    *modelRequest
	terminal *Event
}

type replayRequest struct {
	done            chan struct{}
	events          []OutputEvent
	structured      []json.RawMessage
	structuredStart int
}

type modelRequest struct {
	done chan struct{}
	err  error
}

// DialRunner connects and requires the server-first HELLO frame before
// returning. Missing/legacy protocol zero remains compatible, while an
// explicitly unsupported version fails before any replay or control frame.
func DialRunner(ctx context.Context, socketPath string) (*SocketRunner, error) {
	conn, err := ipc.DialContext(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(helloTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	frame, err := Read(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("runner %s did not send HELLO: %w", socketPath, err)
	}
	if frame.Type != Hello {
		_ = conn.Close()
		return nil, fmt.Errorf("runner %s sent %02x before HELLO", socketPath, byte(frame.Type))
	}
	var info RunnerInfo
	if err := json.Unmarshal(frame.Payload, &info); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("decode runner HELLO: %w", err)
	}
	if info.ID == "" {
		_ = conn.Close()
		return nil, errors.New("runner HELLO has empty id")
	}
	if !IsCompatibleVersion(info.ProtocolVersion) {
		_ = conn.Close()
		return nil, IncompatibleVersionError(info.ProtocolVersion)
	}
	info.SocketPath = socketPath
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	runner := &SocketRunner{conn: conn, info: info, subs: make(map[uint64]chan Event)}
	return runner, nil
}

func (r *SocketRunner) Info() RunnerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	info.Args = append([]string(nil), info.Args...)
	return info
}

func (r *SocketRunner) Replay(ctx context.Context, after uint32) ReplayWindow {
	r.replayMu.Lock()
	defer r.replayMu.Unlock()
	r.startReader()

	request := &replayRequest{done: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		current := r.info.CurrentSeq
		r.mu.Unlock()
		return ReplayWindow{Oldest: current + 1, Current: current}
	}
	r.replay = request
	r.mu.Unlock()

	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], after)
	if err := r.write(ReplayReq, payload[:]); err != nil {
		r.finishReplay(request)
	}

	select {
	case <-request.done:
	case <-ctx.Done():
		r.finishReplay(request)
	case <-time.After(10 * time.Second):
		r.finishReplay(request)
	}

	r.mu.Lock()
	events := append([]OutputEvent(nil), request.events...)
	structured := request.cloneStructured()
	current := r.info.CurrentSeq
	if r.replay == request {
		r.replay = nil
	}
	r.mu.Unlock()
	oldest := current + 1
	if len(events) > 0 {
		oldest = events[0].Seq
	}
	return ReplayWindow{
		Events: events, Structured: structured,
		Gap:     current > after && (len(events) == 0 || after+1 < oldest),
		Oldest:  oldest,
		Current: current,
	}
}

func (r *SocketRunner) Input(_ context.Context, data string) error {
	r.startReader()
	return r.write(Input, []byte(data))
}

// Approve forwards a decision for an approval the runner is holding open.
// The runner answers with an approval_resolved event on the structured
// stream, so no reply frame is needed.
func (r *SocketRunner) Approve(_ context.Context, control ApprovalControl) error {
	if r.Info().ProtocolVersion < 3 {
		return fmt.Errorf("runner protocol v%d cannot route approvals; end and resume the session so it starts on the current runner", r.Info().ProtocolVersion)
	}
	payload, err := EncodeApprovalControl(control)
	if err != nil {
		return err
	}
	r.startReader()
	return r.write(Approve, payload)
}

func (r *SocketRunner) Retry(ctx context.Context) error {
	return r.retryControl(ctx, RetryReq)
}

func (r *SocketRunner) StopRetry(ctx context.Context) error {
	return r.retryControl(ctx, RetryStop)
}

func (r *SocketRunner) retryControl(ctx context.Context, typ Type) error {
	r.retryMu.Lock()
	defer r.retryMu.Unlock()
	if r.Info().ProtocolVersion < 4 {
		return fmt.Errorf("runner protocol v%d cannot control provider retries; update Sessions and start or resume this conversation with the current runtime", r.Info().ProtocolVersion)
	}
	r.startReader()
	request := &modelRequest{done: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return net.ErrClosed
	}
	r.retry = request
	r.mu.Unlock()
	if err := r.write(typ, nil); err != nil {
		r.finishRetry(request, err)
	}
	select {
	case <-request.done:
	case <-ctx.Done():
		r.finishRetry(request, ctx.Err())
	case <-time.After(5 * time.Second):
		r.finishRetry(request, errors.New("runner retry control timed out"))
	}
	r.mu.Lock()
	if r.retry == request {
		r.retry = nil
	}
	err := request.err
	r.mu.Unlock()
	return err
}

func (r *SocketRunner) ConfigureModel(ctx context.Context, control ModelControl) error {
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if r.Info().ProtocolVersion < 2 {
		return fmt.Errorf("runner protocol v%d does not support model control", r.Info().ProtocolVersion)
	}
	payload, err := EncodeModelControl(control)
	if err != nil {
		return err
	}
	r.startReader()
	request := &modelRequest{done: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return net.ErrClosed
	}
	r.model = request
	r.mu.Unlock()
	if err := r.write(ModelReq, payload); err != nil {
		r.finishModel(request, err)
	}
	select {
	case <-request.done:
	case <-ctx.Done():
		r.finishModel(request, ctx.Err())
	case <-time.After(5 * time.Second):
		r.finishModel(request, errors.New("runner model control timed out"))
	}
	r.mu.Lock()
	if r.model == request {
		r.model = nil
	}
	err = request.err
	r.mu.Unlock()
	return err
}

func (r *SocketRunner) Resize(_ context.Context, cols, rows int) error {
	r.startReader()
	payload, err := json.Marshal(struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}{cols, rows})
	if err != nil {
		return err
	}
	return r.write(Resize, payload)
}

func (r *SocketRunner) Kill(context.Context) error {
	r.startReader()
	return r.write(Kill, nil)
}

// HasExited distinguishes a clean runner exit from a dead control socket.
// Session shutdown uses it to make an explicit End idempotent without
// mistaking a lost runner for one that has actually finished.
func (r *SocketRunner) HasExited() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exited
}

func (r *SocketRunner) Subscribe() (<-chan Event, func()) {
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	stream := make(chan Event, 512)
	if r.terminal != nil {
		stream <- *r.terminal
		close(stream)
		r.mu.Unlock()
		return stream, func() {}
	}
	r.subs[id] = stream
	r.mu.Unlock()
	r.startReader()
	return stream, func() {
		r.mu.Lock()
		if existing, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(existing)
		}
		r.mu.Unlock()
	}
}

func (r *SocketRunner) startReader() {
	r.readOnce.Do(func() { go r.readLoop() })
}

func (r *SocketRunner) write(typ Type, payload []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	_ = r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := Write(r.conn, typ, payload)
	_ = r.conn.SetWriteDeadline(time.Time{})
	return err
}

func (r *SocketRunner) readLoop() {
	cleanExit := false
	for {
		frame, err := Read(r.conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// The browser contract intentionally exposes only runner-lost,
				// not transport-specific socket errors.
			}
			r.closeWithLoss(cleanExit)
			return
		}
		cleanExit = r.handleFrame(frame) || cleanExit
	}
}

func (r *SocketRunner) handleFrame(frame Frame) bool {
	switch frame.Type {
	case Output:
		seq, data, err := DecodeOutput(frame.Payload)
		if err != nil {
			_ = r.conn.Close()
			return false
		}
		event := Event{Kind: EventOutput, Output: OutputEvent{Seq: seq, Data: string(data), At: time.Now().UnixMilli()}}
		r.mu.Lock()
		r.info.CurrentSeq = seq
		if r.replay != nil {
			r.replay.events = append(r.replay.events, event.Output)
		}
		r.broadcastLocked(event, false)
		r.mu.Unlock()
	case Exit:
		var exit ExitEvent
		if err := json.Unmarshal(frame.Payload, &exit); err != nil {
			_ = r.conn.Close()
			return false
		}
		r.mu.Lock()
		r.exited = true
		r.info.CurrentSeq = exit.Seq
		event := Event{Kind: EventExit, Exit: exit}
		r.terminal = &event
		r.broadcastLocked(event, true)
		r.mu.Unlock()
		_ = r.conn.Close()
		return true
	case ReplayDone:
		r.mu.Lock()
		request := r.replay
		r.mu.Unlock()
		if request != nil {
			r.finishReplay(request)
		}
	case Structured:
		raw := append(json.RawMessage(nil), frame.Payload...)
		r.mu.Lock()
		if r.replay != nil {
			r.replay.appendStructured(raw)
		} else {
			r.broadcastLocked(Event{Kind: EventCodex, CodexEvent: raw}, false)
		}
		r.mu.Unlock()
	case ModelRes:
		r.handleModelResponse(frame.Payload)
	case RetryState:
		var status ProviderRetryState
		if json.Unmarshal(frame.Payload, &status) != nil {
			_ = r.conn.Close()
			return false
		}
		r.mu.Lock()
		r.broadcastLocked(Event{Kind: EventRetry, Retry: cloneProviderRetry(status.Retry)}, false)
		r.mu.Unlock()
	case RetryRes:
		r.handleRetryResponse(frame.Payload)
	case Hello, SnapshotRes:
		// HELLO is consumed during DialRunner. Extra HELLO and legacy
		// snapshot replies are harmless forward-compatible traffic.
	default:
	}
	return false
}

func (r *SocketRunner) handleModelResponse(payload []byte) {
	var result ModelControlResult
	err := json.Unmarshal(payload, &result)
	if err == nil && result.Error != "" {
		err = errors.New(result.Error)
	}
	r.mu.Lock()
	request := r.model
	r.mu.Unlock()
	if request != nil {
		r.finishModel(request, err)
	}
}

func (r *SocketRunner) handleRetryResponse(payload []byte) {
	var result RetryControlResult
	err := json.Unmarshal(payload, &result)
	if err == nil && result.Error != "" {
		err = errors.New(result.Error)
	}
	r.mu.Lock()
	request := r.retry
	r.mu.Unlock()
	if request != nil {
		r.finishRetry(request, err)
	}
}

func (r *replayRequest) appendStructured(raw json.RawMessage) {
	if len(r.structured) < MaxStructuredReplayEvents {
		r.structured = append(r.structured, raw)
		return
	}
	r.structured[r.structuredStart] = raw
	r.structuredStart = (r.structuredStart + 1) % len(r.structured)
}

func (r *replayRequest) cloneStructured() []json.RawMessage {
	structured := make([]json.RawMessage, len(r.structured))
	for index := range structured {
		source := (r.structuredStart + index) % len(r.structured)
		structured[index] = append(json.RawMessage(nil), r.structured[source]...)
	}
	return structured
}

func (r *SocketRunner) finishModel(request *modelRequest, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.model != request {
		return
	}
	request.err = err
	select {
	case <-request.done:
	default:
		close(request.done)
	}
}

func (r *SocketRunner) finishRetry(request *modelRequest, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retry != request {
		return
	}
	request.err = err
	select {
	case <-request.done:
	default:
		close(request.done)
	}
}

func cloneProviderRetry(value *ProviderRetry) *ProviderRetry {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (r *SocketRunner) finishReplay(request *replayRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replay != request {
		return
	}
	select {
	case <-request.done:
	default:
		close(request.done)
	}
}

func (r *SocketRunner) closeWithLoss(cleanExit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.replay != nil {
		select {
		case <-r.replay.done:
		default:
			close(r.replay.done)
		}
	}
	if r.model != nil {
		r.model.err = net.ErrClosed
		select {
		case <-r.model.done:
		default:
			close(r.model.done)
		}
	}
	if r.retry != nil {
		r.retry.err = net.ErrClosed
		select {
		case <-r.retry.done:
		default:
			close(r.retry.done)
		}
	}
	if !cleanExit && !r.exited {
		event := Event{Kind: EventRunnerLost, Exit: ExitEvent{Seq: r.info.CurrentSeq, Reason: "runner-lost"}}
		r.terminal = &event
		r.broadcastLocked(event, true)
		return
	}
	for id, stream := range r.subs {
		close(stream)
		delete(r.subs, id)
	}
}

func (r *SocketRunner) broadcastLocked(event Event, terminal bool) {
	for _, stream := range r.subs {
		select {
		case stream <- event:
		default:
			if terminal {
				select {
				case <-stream:
				default:
				}
				stream <- event
			}
		}
	}
	if terminal {
		for id, stream := range r.subs {
			close(stream)
			delete(r.subs, id)
		}
	}
}
