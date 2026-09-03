// Package prototest provides an in-memory runner implementation for daemon
// integration tests. It never spawns a process or touches a Unix socket.
package prototest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

type Launcher struct {
	mu       sync.Mutex
	Runners  map[string]*Runner
	Launches []proto.LaunchRequest
	// Wakes records the paused sessions the daemon asked to restart.
	Wakes   []string
	WakeErr error
	PID     int
	Err     error
}

func NewLauncher() *Launcher {
	return &Launcher{Runners: make(map[string]*Runner), PID: 4242}
}

func (l *Launcher) ProgramArguments(proto.LaunchRequest) []string {
	return []string{"/usr/bin/true"}
}

func (l *Launcher) Launch(_ context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Err != nil {
		return nil, l.Err
	}
	request.Info.PID = l.PID
	request.Info.ProtocolVersion = proto.ProtocolVersion
	runner := NewRunner(request.Info)
	l.Runners[request.Info.ID] = runner
	l.Launches = append(l.Launches, request)
	return runner, nil
}

func (l *Launcher) Attach(_ context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	runner, ok := l.Runners[info.ID]
	if !ok {
		return nil, errors.New("fake runner is not available")
	}
	return runner, nil
}

// Wake stands in for a launchd kickstart: a fresh fake runner answers for
// the paused id from then on.
func (l *Launcher) Wake(_ context.Context, id string) (proto.Runner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Wakes = append(l.Wakes, id)
	if l.WakeErr != nil {
		return nil, l.WakeErr
	}
	runner := NewRunner(proto.RunnerInfo{ID: id, PID: l.PID, ProtocolVersion: proto.ProtocolVersion, Cols: 120, Rows: 40})
	l.Runners[id] = runner
	return runner, nil
}

func (l *Launcher) Runner(id string) *Runner {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Runners[id]
}

type Runner struct {
	mu          sync.Mutex
	info        proto.RunnerInfo
	outputs     []proto.OutputEvent
	structured  []json.RawMessage
	inputs      []string
	approvals   []proto.ApprovalControl
	models      []proto.ModelControl
	retries     int
	retryStops  int
	cols        int
	rows        int
	exited      bool
	subscribers map[uint64]chan proto.Event
	nextSubID   uint64
	changes     chan struct{}
}

func NewRunner(info proto.RunnerInfo) *Runner {
	return &Runner{
		info: info, cols: info.Cols, rows: info.Rows,
		subscribers: make(map[uint64]chan proto.Event),
		changes:     make(chan struct{}, 1),
	}
}

func (r *Runner) Info() proto.RunnerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	info.Args = append([]string(nil), info.Args...)
	info.CurrentSeq = r.currentSeqLocked()
	return info
}

func (r *Runner) Replay(_ context.Context, after uint32) proto.ReplayWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := make([]proto.OutputEvent, 0, len(r.outputs))
	for _, event := range r.outputs {
		if event.Seq > after {
			events = append(events, event)
		}
	}
	oldest := r.currentSeqLocked() + 1
	if len(r.outputs) > 0 {
		oldest = r.outputs[0].Seq
	}
	return proto.ReplayWindow{
		Events: events, Structured: cloneRaw(r.structured), Gap: len(r.outputs) > 0 && after+1 < oldest,
		Oldest: oldest, Current: r.currentSeqLocked(),
	}
}

func (r *Runner) Input(_ context.Context, data string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.inputs = append(r.inputs, data)
	r.signalChangeLocked()
	return nil
}

func (r *Runner) ConfigureModel(_ context.Context, control proto.ModelControl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.models = append(r.models, control)
	r.signalChangeLocked()
	return nil
}

func (r *Runner) Approve(_ context.Context, control proto.ApprovalControl) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.approvals = append(r.approvals, control)
	r.signalChangeLocked()
	return nil
}

func (r *Runner) Retry(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.retries++
	r.signalChangeLocked()
	return nil
}

func (r *Runner) StopRetry(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.retryStops++
	r.signalChangeLocked()
	return nil
}

// Approvals returns every approval decision the daemon sent, in order.
func (r *Runner) Approvals() []proto.ApprovalControl {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.ApprovalControl(nil), r.approvals...)
}

func (r *Runner) Resize(_ context.Context, cols, rows int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exited {
		return errors.New("runner exited")
	}
	r.cols, r.rows = cols, rows
	r.signalChangeLocked()
	return nil
}

func (r *Runner) Kill(context.Context) error {
	zero := 0
	r.Emit(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &zero, Seq: r.CurrentSeq()}})
	return nil
}

func (r *Runner) HasExited() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exited
}

func (r *Runner) Subscribe() (<-chan proto.Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextSubID
	r.nextSubID++
	stream := make(chan proto.Event, 512)
	if r.exited {
		close(stream)
		return stream, func() {}
	}
	r.subscribers[id] = stream
	return stream, func() {
		r.mu.Lock()
		if existing, ok := r.subscribers[id]; ok {
			delete(r.subscribers, id)
			close(existing)
		}
		r.mu.Unlock()
	}
}

func (r *Runner) AddOutput(data string) proto.OutputEvent {
	r.mu.Lock()
	seq := r.currentSeqLocked() + 1
	r.mu.Unlock()
	event := proto.OutputEvent{Seq: seq, Data: data, At: time.Now().UnixMilli()}
	r.Emit(proto.Event{Kind: proto.EventOutput, Output: event})
	return event
}

func (r *Runner) AddClaudeEvent(value any) {
	encoded, _ := json.Marshal(value)
	r.Emit(proto.Event{Kind: proto.EventClaude, ClaudeEvent: encoded})
}

func (r *Runner) AddCodexEvent(value any) {
	encoded, _ := json.Marshal(value)
	r.Emit(proto.Event{Kind: proto.EventCodex, CodexEvent: encoded})
}

func (r *Runner) SetRetry(value *proto.ProviderRetry) {
	r.Emit(proto.Event{Kind: proto.EventRetry, Retry: value})
}

func (r *Runner) Emit(event proto.Event) {
	r.mu.Lock()
	if event.Kind == proto.EventOutput {
		r.outputs = append(r.outputs, event.Output)
	}
	if event.Kind == proto.EventCodex {
		r.structured = append(r.structured, append(json.RawMessage(nil), event.CodexEvent...))
	}
	terminal := event.Kind == proto.EventExit || event.Kind == proto.EventRunnerLost
	if terminal {
		r.exited = true
	}
	for _, stream := range r.subscribers {
		stream <- event
	}
	if terminal {
		for id, stream := range r.subscribers {
			close(stream)
			delete(r.subscribers, id)
		}
	}
	r.signalChangeLocked()
	r.mu.Unlock()
}

func cloneRaw(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = append(json.RawMessage(nil), values[index]...)
	}
	return result
}

// Changes reports coalesced state transitions so tests can synchronize with
// fake-runner work without scheduler sleeps. Callers must always re-read the
// state they care about after a notification.
func (r *Runner) Changes() <-chan struct{} { return r.changes }

func (r *Runner) signalChangeLocked() {
	select {
	case r.changes <- struct{}{}:
	default:
	}
}

func (r *Runner) Inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inputs...)
}

func (r *Runner) ModelControls() []proto.ModelControl {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.ModelControl(nil), r.models...)
}

func (r *Runner) RetryControls() (run, stop int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retries, r.retryStops
}

func (r *Runner) Size() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cols, r.rows
}

func (r *Runner) CurrentSeq() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentSeqLocked()
}

func (r *Runner) currentSeqLocked() uint32 {
	if len(r.outputs) == 0 {
		return 0
	}
	return r.outputs[len(r.outputs)-1].Seq
}
