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
	mu       sync.Mutex
	info     RunnerInfo
	exited   bool
	closed   bool
	subs     map[uint64]chan Event
	nextSub  uint64
	replay   *replayRequest
	model    *modelRequest
	terminal *Event
}

type replayRequest struct {
	done       chan struct{}
	events     []OutputEvent
	structured []json.RawMessage
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
	structured := make([]json.RawMessage, len(request.structured))
	for index := range request.structured {
		structured[index] = append(json.RawMessage(nil), request.structured[index]...)
	}
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
		switch frame.Type {
		case Output:
			seq, data, err := DecodeOutput(frame.Payload)
			if err != nil {
				_ = r.conn.Close()
				continue
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
				continue
			}
			r.mu.Lock()
			r.exited = true
			r.info.CurrentSeq = exit.Seq
			event := Event{Kind: EventExit, Exit: exit}
			r.terminal = &event
			r.broadcastLocked(event, true)
			r.mu.Unlock()
			cleanExit = true
			_ = r.conn.Close()
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
				r.replay.structured = append(r.replay.structured, raw)
			} else {
				r.broadcastLocked(Event{Kind: EventCodex, CodexEvent: raw}, false)
			}
			r.mu.Unlock()
		case ModelRes:
			var result ModelControlResult
			err := json.Unmarshal(frame.Payload, &result)
			if err == nil && result.Error != "" {
				err = errors.New(result.Error)
			}
			r.mu.Lock()
			request := r.model
			r.mu.Unlock()
			if request != nil {
				r.finishModel(request, err)
			}
		case Hello, SnapshotRes:
			// HELLO is consumed during DialRunner. Extra HELLO and legacy
			// snapshot replies are harmless forward-compatible traffic.
		default:
		}
	}
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
