package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

type pausingMuxInput struct {
	sessionService
	started, release chan struct{}
	once             sync.Once
}

type cancelableMuxRunner struct {
	*prototest.Runner
	started, release chan struct{}
}

type panickingMuxInput struct {
	sessionService
	started, release chan struct{}
	once             sync.Once
}

func (p *panickingMuxInput) Input(context.Context, string, string) bool {
	p.once.Do(func() { close(p.started); <-p.release })
	panic("fixture runner failure")
}

func TestMuxWorkerFailureClosesOnlyItsConnectionAndDropsPendingInput(t *testing.T) {
	var failing *panickingMuxInput
	f := newMuxWorkFixture(t, func(f *muxWorkFixture) {
		failing = &panickingMuxInput{sessionService: f.registry, started: make(chan struct{}), release: make(chan struct{})}
		f.handler.registry = failing
	})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(failing.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "input", "sessionId": "terminal-b", "requestId": "failure", "data": "first"})
	select {
	case <-failing.started:
	case <-ctx.Done():
		t.Fatal("input did not start")
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "input", "sessionId": "terminal-b", "data": "must not run"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatalf("read loop stalled: %v", message)
	}
	release()
	select {
	case <-f.done:
	case <-ctx.Done():
		t.Fatal("failed connection did not stop")
	}
	if f.handler.submits.tracked() != 0 || len(f.b.Inputs()) != 0 {
		t.Fatal("worker failure retained lock or forwarded pending input")
	}
	response, err := http.Get(f.server.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("daemon no longer serves health: %d", response.StatusCode)
	}
}

func (r *cancelableMuxRunner) Input(ctx context.Context, data string) error {
	if err := r.Runner.Input(ctx, data); err != nil {
		return err
	}
	close(r.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

func TestMuxCloseCancelsInputBeforeDetachingSession(t *testing.T) {
	daemon := newTestDaemon(t)
	runner := &cancelableMuxRunner{Runner: prototest.NewRunner(proto.RunnerInfo{ID: "attached", Cmd: "/bin/sh", Cwd: daemon.root, Cols: 120, Rows: 40, ProtocolVersion: 2}), started: make(chan struct{}), release: make(chan struct{})}
	session, err := daemon.registry.Register(context.Background(), runner, "Attached fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { daemon.handler.ServeHTTP(w, r); close(done) }))
	t.Cleanup(server.Close)
	// Also release on failure so this owned fixture cannot strand cleanup.
	t.Cleanup(func() { close(runner.release) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws?mux=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	writeWS(t, ctx, socket, map[string]any{"type": "attach", "sessionId": "attached", "outputReplay": false})
	if message := readWS(t, ctx, socket); message["type"] != "hello" {
		t.Fatalf("hello=%v", message)
	}
	writeWS(t, ctx, socket, map[string]any{"type": "input", "sessionId": "attached", "data": "owned input"})
	select {
	case <-runner.started:
	case <-ctx.Done():
		t.Fatal("input did not begin")
	}
	_ = socket.CloseNow()
	select {
	case <-done:
	case <-time.After(700 * time.Millisecond):
		t.Fatal("attachment cleanup blocked cancellation of active input")
	}
	if daemon.handler.submits.tracked() != 0 {
		t.Fatal("input retained session lock after close")
	}
}

func (p *pausingMuxInput) Input(ctx context.Context, id, data string) bool {
	written := p.sessionService.Input(ctx, id, data)
	if strings.HasPrefix(data, "\x1b[200~") {
		p.once.Do(func() {
			close(p.started)
			select {
			case <-p.release:
			case <-ctx.Done():
			}
		})
	}
	return written
}

func newPausedMuxWorkFixture(t *testing.T) (muxWorkFixture, *pausingMuxInput, func()) {
	t.Helper()
	var pause *pausingMuxInput
	f := newMuxWorkFixture(t, func(f *muxWorkFixture) {
		pause = &pausingMuxInput{sessionService: f.registry, started: make(chan struct{}), release: make(chan struct{})}
		f.handler.registry = pause
	})
	var once sync.Once
	release := func() { once.Do(func() { close(pause.release) }) }
	t.Cleanup(release)
	return f, pause, release
}

func waitSessionLockUsers(t *testing.T, locks *sessionMutexes, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		locks.mu.Lock()
		entry := locks.entries[id]
		got := 0
		if entry != nil {
			got = entry.users
		}
		locks.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not reach %d holders/waiters", id, want)
}

func TestMuxSubmitExcludesRawWritesFromHTTPAndSingleSockets(t *testing.T) {
	for _, mode := range []string{"HTTP", "single-json", "single-binary", "single-text"} {
		t.Run(mode, func(t *testing.T) {
			f, pause, release := newPausedMuxWorkFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "submit", "data": "whole message"})
			select {
			case <-pause.started:
			case <-ctx.Done():
				t.Fatal("paste did not start")
			}
			result := make(chan error, 1)
			if mode == "HTTP" {
				go func() {
					response, err := http.Post(f.server.URL+"/api/sessions/legacy-a/input", "application/json", strings.NewReader(`{"data":"external"}`))
					if err == nil {
						body, _ := io.ReadAll(response.Body)
						response.Body.Close()
						if response.StatusCode != http.StatusOK {
							err = fmt.Errorf("raw HTTP: %d %s", response.StatusCode, body)
						}
					}
					result <- err
				}()
			} else {
				single, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/ws?sessionId=legacy-a", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer single.CloseNow()
				if message := readWS(t, ctx, single); message["type"] != "hello" {
					t.Fatalf("single hello=%v", message)
				}
				switch mode {
				case "single-json":
					writeWS(t, ctx, single, map[string]any{"type": "input", "data": "external"})
				case "single-binary":
					err = single.Write(ctx, websocket.MessageBinary, []byte("external"))
				case "single-text":
					err = single.Write(ctx, websocket.MessageText, []byte("external"))
				}
				if err != nil {
					t.Fatal(err)
				}
				result <- nil
			}
			waitSessionLockUsers(t, f.handler.submits, "legacy-a", 2)
			if len(f.a.Inputs()) != 1 {
				t.Fatalf("raw input split paste and Enter: %q", f.a.Inputs())
			}
			release()
			awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 2 })
			recordUserEvent(t, f.legacy, time.Now(), "whole message")
			awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 3 })
			if want := []string{"\x1b[200~whole message\x1b[201~", "\r", "external"}; !reflect.DeepEqual(f.a.Inputs(), want) {
				t.Fatalf("wire order=%q", f.a.Inputs())
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("external input did not finish")
			}
		})
	}
}

func TestMuxCloseDuringPasteNeverSendsEnterOrLaterCommands(t *testing.T) {
	f, pause, _ := newPausedMuxWorkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "one", "data": "pending"})
	select {
	case <-pause.started:
	case <-ctx.Done():
		t.Fatal("paste did not start")
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "two", "data": "never send"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatalf("receive loop stalled: %v", message)
	}
	_ = f.socket.CloseNow()
	select {
	case <-f.done:
	case <-time.After(700 * time.Millisecond):
		t.Fatal("paste cancellation did not stop workers")
	}
	if len(f.a.Inputs()) != 1 || f.handler.submits.tracked() != 0 {
		t.Fatalf("input continued after close: %q", f.a.Inputs())
	}
}
