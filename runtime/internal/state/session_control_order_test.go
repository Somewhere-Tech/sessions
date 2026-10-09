package state

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

// Input and model frames are ordered by the session's control admission, not
// by its state lock, so readers and the event pump are not held behind runner
// I/O. These tests hold a frame inside a gated fake runner and prove what can
// and cannot happen meanwhile. Every wait is on a signal from the code under
// test; the timeouts only bound a failure.

const controlWaitLimit = 3 * time.Second

// gate holds one kind of runner call until released.
type gate struct {
	entered chan string
	release chan struct{}
	once    sync.Once
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// gatedRunner is the in-memory fake runner with chosen calls held open. It
// records the order in which frames reached the runner.
type gatedRunner struct {
	*prototest.Runner
	protocol int

	mu       sync.Mutex
	gates    map[string]*gate
	frames   []string
	modelErr error
}

func (g *gatedRunner) hold(t *testing.T, kind string) *gate {
	t.Helper()
	held := &gate{entered: make(chan string, 8), release: make(chan struct{})}
	g.mu.Lock()
	g.gates[kind] = held
	g.mu.Unlock()
	t.Cleanup(held.open)
	return held
}

func (g *gatedRunner) pass(kind, label string) {
	g.mu.Lock()
	held := g.gates[kind]
	g.mu.Unlock()
	if held != nil {
		held.entered <- label
		<-held.release
	}
}

func (g *gatedRunner) record(frame string) {
	g.mu.Lock()
	g.frames = append(g.frames, frame)
	g.mu.Unlock()
}

func (g *gatedRunner) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.frames...)
}

func (g *gatedRunner) Info() proto.RunnerInfo {
	info := g.Runner.Info()
	if g.protocol != 0 {
		info.ProtocolVersion = g.protocol
	}
	return info
}

func (g *gatedRunner) Input(ctx context.Context, data string) error {
	g.pass("input", data)
	g.record("input:" + data)
	return g.Runner.Input(ctx, data)
}

func (g *gatedRunner) ConfigureModel(ctx context.Context, control proto.ModelControl) error {
	g.pass("model", control.Model)
	g.record("model:" + control.Model)
	g.mu.Lock()
	failure := g.modelErr
	g.mu.Unlock()
	if failure != nil {
		return failure
	}
	if err := g.Runner.ConfigureModel(ctx, control); err != nil {
		return err
	}
	// Held after the runner acknowledged, for the exit-during-response case.
	g.pass("model-acknowledged", control.Model)
	return nil
}

func (g *gatedRunner) Approve(ctx context.Context, control proto.ApprovalControl) error {
	g.record("approve:" + control.ID)
	return g.Runner.Approve(ctx, control)
}

type gatedLauncher struct {
	*prototest.Launcher
	protocol int
	runner   *gatedRunner
}

func (l *gatedLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	inner, err := l.Launcher.Launch(ctx, request)
	if err != nil {
		return nil, err
	}
	l.runner = &gatedRunner{Runner: inner.(*prototest.Runner), protocol: l.protocol, gates: make(map[string]*gate)}
	return l.runner, nil
}

func gatedSession(t *testing.T, protocol int) (*Registry, *Session, *gatedRunner) {
	t.Helper()
	root := t.TempDir()
	launcher := &gatedLauncher{Launcher: prototest.NewLauncher(), protocol: protocol}
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, launcher)
	created, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := registry.Get(created.ID)
	if !ok || launcher.runner == nil {
		t.Fatalf("session %s was not created through the gated runner", created.ID)
	}
	t.Cleanup(func() { _ = session.Close() })
	return registry, session, launcher.runner
}

func within[T any](t *testing.T, what string, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(controlWaitLimit):
		t.Fatalf("%s did not finish while a runner frame was held", what)
		var zero T
		return zero
	}
}

func entered(t *testing.T, held *gate) string {
	t.Helper()
	return within(t, "the held runner call", held.entered)
}

// observeOutput waits until the session has applied and published output.
func observeOutput(t *testing.T, events <-chan proto.Event, data string) {
	t.Helper()
	deadline := time.After(controlWaitLimit)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("the session stopped publishing before the output arrived")
			}
			if event.Kind == proto.EventOutput && event.Output.Data == data {
				return
			}
		case <-deadline:
			t.Fatalf("the event pump did not apply %q while a runner frame was held", data)
		}
	}
}

// observeExit waits until the session has applied its exit.
func observeExit(t *testing.T, events <-chan proto.Event) {
	t.Helper()
	deadline := time.After(controlWaitLimit)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the session did not apply its exit")
		}
	}
}

func TestModelFrameInFlightDoesNotBlockReadersOrTheEventPump(t *testing.T) {
	registry, session, runner := gatedSession(t, 0)
	attached := session.Attach(AttachOptions{})
	defer attached.Cancel()
	held := runner.hold(t, "model")

	configured := make(chan error, 1)
	go func() { configured <- session.ConfigureModel(context.Background(), "next-model", "high") }()
	entered(t, held)

	read := make(chan SessionInfo, 1)
	go func() { read <- session.Info() }()
	within(t, "Info", read)
	listed := make(chan int, 1)
	go func() { listed <- len(registry.List(true)) }()
	if count := within(t, "Registry.List", listed); count != 1 {
		t.Fatalf("listed %d sessions, want 1", count)
	}
	go runner.AddOutput("during-model-frame")
	observeOutput(t, attached.Events, "during-model-frame")

	held.open()
	if err := within(t, "ConfigureModel", configured); err != nil {
		t.Fatal(err)
	}
	if info := session.Info(); info.Model != "next-model" || info.Effort != "high" {
		t.Fatalf("acknowledged change recorded as %q/%q", info.Model, info.Effort)
	}
}

func TestInputWriteInFlightDoesNotBlockReadersOrTheEventPump(t *testing.T) {
	registry, session, runner := gatedSession(t, 0)
	attached := session.Attach(AttachOptions{})
	defer attached.Cancel()
	held := runner.hold(t, "input")

	sent := make(chan bool, 1)
	go func() { sent <- session.Input(context.Background(), "a long paste") }()
	entered(t, held)

	go runner.AddOutput("during-input-write")
	observeOutput(t, attached.Events, "during-input-write")
	working := make(chan bool, 1)
	go func() { previous, _ := session.SetWorking(true); working <- previous }()
	within(t, "SetWorking", working)
	listed := make(chan []SessionInfo, 1)
	go func() { listed <- registry.List(true) }()
	if infos := within(t, "Registry.List", listed); len(infos) != 1 || !infos[0].Working {
		t.Fatalf("listing during the input write = %#v, want the recorded working state", infos)
	}

	held.open()
	if !within(t, "Input", sent) {
		t.Fatal("the held input was not delivered after release")
	}
}

func TestInputAdmittedAfterAModelChangeReachesTheRunnerAfterIt(t *testing.T) {
	_, session, runner := gatedSession(t, 0)
	held := runner.hold(t, "model")
	configured := make(chan error, 1)
	go func() { configured <- session.ConfigureModel(context.Background(), "next-model", "") }()
	entered(t, held)

	sent := make(chan bool, 1)
	go func() { sent <- session.Input(context.Background(), "after the model") }()
	// The input cannot have reached the runner while the model frame is held.
	if frames := runner.recorded(); len(frames) != 0 {
		t.Fatalf("frames before the model frame completed = %v", frames)
	}
	held.open()
	if err := within(t, "ConfigureModel", configured); err != nil {
		t.Fatal(err)
	}
	if !within(t, "Input", sent) {
		t.Fatal("input was not delivered")
	}
	if frames := runner.recorded(); len(frames) != 2 || frames[0] != "model:next-model" || frames[1] != "input:after the model" {
		t.Fatalf("frames = %v, want the model change before the later input", frames)
	}
}

func TestASecondModelChangeIsRecordedLast(t *testing.T) {
	_, session, runner := gatedSession(t, 0)
	held := runner.hold(t, "model")
	first := make(chan error, 1)
	go func() { first <- session.ConfigureModel(context.Background(), "model-a", "") }()
	if label := entered(t, held); label != "model-a" {
		t.Fatalf("first held frame = %q", label)
	}
	second := make(chan error, 1)
	go func() { second <- session.ConfigureModel(context.Background(), "model-b", "") }()

	held.open()
	if err := within(t, "first ConfigureModel", first); err != nil {
		t.Fatal(err)
	}
	if err := within(t, "second ConfigureModel", second); err != nil {
		t.Fatal(err)
	}
	if frames := runner.recorded(); len(frames) != 2 || frames[0] != "model:model-a" || frames[1] != "model:model-b" {
		t.Fatalf("frames = %v, want A then B", frames)
	}
	if info := session.Info(); info.Model != "model-b" {
		t.Fatalf("recorded model = %q, want the later change", info.Model)
	}
}

func TestModelChangeRefusalsSendNoFrame(t *testing.T) {
	t.Run("old runner", func(t *testing.T) {
		_, session, runner := gatedSession(t, 1)
		if err := session.ConfigureModel(context.Background(), "next-model", ""); !errors.Is(err, ErrRunnerProtocol) {
			t.Fatalf("old runner = %v", err)
		}
		if frames := runner.recorded(); len(frames) != 0 {
			t.Fatalf("frames = %v", frames)
		}
	})
	t.Run("working", func(t *testing.T) {
		_, session, runner := gatedSession(t, 0)
		session.SetWorking(true)
		if err := session.ConfigureModel(context.Background(), "next-model", ""); !errors.Is(err, ErrSessionWorking) {
			t.Fatalf("working session = %v", err)
		}
		if frames := runner.recorded(); len(frames) != 0 {
			t.Fatalf("frames = %v", frames)
		}
	})
	t.Run("exited", func(t *testing.T) {
		_, session, runner := gatedSession(t, 0)
		attached := session.Attach(AttachOptions{})
		_ = runner.Kill(context.Background())
		observeExit(t, attached.Events)
		if err := session.ConfigureModel(context.Background(), "next-model", ""); !errors.Is(err, ErrSessionEnded) {
			t.Fatalf("exited session = %v", err)
		}
		if frames := runner.recorded(); len(frames) != 0 {
			t.Fatalf("frames = %v", frames)
		}
	})
}

func TestAFailedModelChangeKeepsTheRecord(t *testing.T) {
	_, session, runner := gatedSession(t, 0)
	before := session.Info()
	runner.mu.Lock()
	runner.modelErr = errors.New("metadata write failed")
	runner.mu.Unlock()
	if err := session.ConfigureModel(context.Background(), "next-model", "high"); err == nil {
		t.Fatal("a refused model change reported success")
	}
	after := session.Info()
	if after.Model != before.Model || after.Effort != before.Effort || len(after.Args) != len(before.Args) {
		t.Fatalf("record changed by a failed model change: before %#v after %#v", before, after)
	}
	for index := range before.Args {
		if after.Args[index] != before.Args[index] {
			t.Fatalf("args changed: before %v after %v", before.Args, after.Args)
		}
	}
}

func TestAnExitDuringAnAcknowledgedModelChangeStaysExited(t *testing.T) {
	_, session, runner := gatedSession(t, 0)
	attached := session.Attach(AttachOptions{})
	held := runner.hold(t, "model-acknowledged")
	configured := make(chan error, 1)
	go func() { configured <- session.ConfigureModel(context.Background(), "next-model", "") }()
	entered(t, held)

	_ = runner.Kill(context.Background())
	observeExit(t, attached.Events)
	held.open()
	if err := within(t, "ConfigureModel", configured); err != nil {
		t.Fatalf("an acknowledged change reported %v", err)
	}
	info := session.Info()
	if !info.Exited || info.Working || info.Model != "next-model" {
		t.Fatalf("after exit during the response: exited=%v working=%v model=%q", info.Exited, info.Working, info.Model)
	}
}

// Approvals and retry controls keep their place in the same order, and taking
// the control admission before the state lock everywhere cannot deadlock.
func TestControlsKeepTheirOrderWithoutDeadlock(t *testing.T) {
	_, session, runner := gatedSession(t, 0)
	held := runner.hold(t, "model")
	configured := make(chan error, 1)
	go func() { configured <- session.ConfigureModel(context.Background(), "next-model", "") }()
	entered(t, held)

	controls := make(chan error, 3)
	go func() { controls <- session.Approve(context.Background(), proto.ApprovalControl{}) }()
	go func() { controls <- session.RetryProvider(context.Background()) }()
	go func() { controls <- session.StopProviderRetry(context.Background()) }()
	sent := make(chan bool, 1)
	go func() { sent <- session.Input(context.Background(), "later") }()
	read := make(chan SessionInfo, 1)
	go func() { read <- session.Info() }()
	within(t, "Info", read)
	select {
	case err := <-controls:
		t.Fatalf("a control completed while the model frame was held: %v", err)
	default:
	}

	held.open()
	if err := within(t, "ConfigureModel", configured); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		// None is eligible here; each answers with its existing refusal.
		if err := within(t, "a control", controls); err == nil {
			t.Fatal("an ineligible control reported success")
		}
	}
	within(t, "Input", sent)
	if frames := runner.recorded(); len(frames) == 0 || frames[0] != "model:next-model" {
		t.Fatalf("frames = %v, want the model change first", frames)
	}
}
