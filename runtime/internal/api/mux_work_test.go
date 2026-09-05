package api

import (
	"context"
	"fmt"
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
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type muxWorkFixture struct {
	testDaemon
	a, b   *prototest.Runner
	legacy *state.Session
	socket *websocket.Conn
	server *httptest.Server
	done   chan struct{}
}

func newMuxWorkFixture(t *testing.T, configure ...func(*muxWorkFixture)) muxWorkFixture {
	t.Helper()
	f := muxWorkFixture{testDaemon: newTestDaemon(t), done: make(chan struct{})}
	f.a = prototest.NewRunner(proto.RunnerInfo{ID: "legacy-a", Cmd: "claude", Cwd: f.root, Cols: 120, Rows: 40, ProtocolVersion: 2})
	f.b = prototest.NewRunner(proto.RunnerInfo{ID: "terminal-b", Cmd: "/bin/sh", Cwd: f.root, Cols: 120, Rows: 40, ProtocolVersion: 2})
	var err error
	f.legacy, err = f.registry.Register(context.Background(), f.a, "Legacy fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.Register(context.Background(), f.b, "Terminal fixture", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.legacy.Close()
		if session, ok := f.registry.Get("terminal-b"); ok {
			_ = session.Close()
		}
	})
	for _, apply := range configure {
		apply(&f)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.ServeHTTP(w, r)
		if r.URL.Path == "/ws" && r.URL.Query().Get("mux") == "1" {
			close(f.done)
		}
	}))
	t.Cleanup(f.server.Close)
	f.socket, _, err = websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.server.URL, "http")+"/ws?mux=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.socket.CloseNow() })
	return f
}

func TestMuxWorkPreservesFIFOAndBoundsOutstandingMessages(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var order []string
	q := newMuxWork(context.Background(), func(ctx context.Context, message clientMessage) {
		if message.Data == "0" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
		}
		order = append(order, message.Data)
		if message.Data == fmt.Sprint(muxWorkMessages-1) {
			close(finished)
		}
	})
	t.Cleanup(q.close)
	for i := 0; i < muxWorkMessages; i++ {
		if !q.enqueue(clientMessage{Type: []string{"submit", "input", "resize"}[i%3], SessionID: "a", Data: fmt.Sprint(i)}) {
			t.Fatalf("refused command %d", i)
		}
	}
	<-started
	if q.enqueue(clientMessage{Type: "input", SessionID: "a", Data: "overflow"}) {
		t.Fatal("message limit ignored")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("FIFO did not drain")
	}
	q.close()
	for i, got := range order {
		if got != fmt.Sprint(i) {
			t.Fatalf("command %d=%q", i, got)
		}
	}
	if len(order) != muxWorkMessages || q.count != 0 || q.bytes != 0 || len(q.sessions) != 0 {
		t.Fatal("commands lost or worker state retained")
	}
}

func TestMuxWorkBoundsConcurrentSessionsAndDropsPendingOnClose(t *testing.T) {
	started := make(chan string, muxWorkSessions)
	q := newMuxWork(context.Background(), func(ctx context.Context, message clientMessage) { started <- message.SessionID; <-ctx.Done() })
	for i := 0; i < muxWorkSessions; i++ {
		if !q.enqueue(clientMessage{Type: "submit", SessionID: fmt.Sprint(i)}) {
			t.Fatal("session slot refused")
		}
	}
	for range muxWorkSessions {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	if q.enqueue(clientMessage{Type: "input", SessionID: "overflow"}) {
		t.Fatal("session worker bound ignored")
	}
	if !q.enqueue(clientMessage{Type: "input", SessionID: "0", Data: "must be dropped"}) {
		t.Fatal("existing session queue refused")
	}
	q.close()
	if len(started) != 0 || q.count != 0 || q.bytes != 0 || len(q.sessions) != 0 {
		t.Fatal("queued command executed during close or leaked")
	}
	if q.enqueue(clientMessage{Type: "input", SessionID: "after-close"}) {
		t.Fatal("closed queue accepted input")
	}
}

func TestMuxWorkByteLimitIncludesExecutingCommands(t *testing.T) {
	started := make(chan struct{})
	q := newMuxWork(context.Background(), func(ctx context.Context, _ clientMessage) { close(started); <-ctx.Done() })
	t.Cleanup(q.close)
	message := clientMessage{Type: "input", SessionID: "a", Data: strings.Repeat("x", 256*1024-32)}
	for range 4 {
		if !q.enqueue(message) {
			t.Fatal("bounded payload refused")
		}
	}
	<-started
	if q.enqueue(message) {
		t.Fatal("byte limit ignored")
	}
	q.close()
	if q.count != 0 || q.bytes != 0 {
		t.Fatal("close retained payload accounting")
	}
}

func TestMuxSameSessionInputSubmitAndResizeRemainOrdered(t *testing.T) {
	f := newMuxWorkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "one", "data": "first"})
	awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 2 })
	for _, message := range []map[string]any{
		{"type": "input", "sessionId": "legacy-a", "requestId": "raw", "data": "raw"},
		{"type": "submit", "sessionId": "legacy-a", "requestId": "two", "data": "second"},
		{"type": "resize", "sessionId": "legacy-a", "cols": 82, "rows": 22},
		{"type": "ping"},
	} {
		writeWS(t, ctx, f.socket, message)
	}
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatalf("same-session command overtook pending submit: %v", message)
	}
	if cols, rows := f.a.Size(); cols != 120 || rows != 40 || len(f.a.Inputs()) != 2 {
		t.Fatal("input/resize interleaved into submit")
	}
	recordUserEvent(t, f.legacy, time.Now(), "first")
	for _, id := range []string{"one", "raw"} {
		if message := readWS(t, ctx, f.socket); message["requestId"] != id || message["ok"] != true {
			t.Fatalf("ack order for %s: %v", id, message)
		}
	}
	awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 5 })
	recordUserEvent(t, f.legacy, time.Now(), "second")
	if message := readWS(t, ctx, f.socket); message["requestId"] != "two" || message["ok"] != true {
		t.Fatalf("second submit=%v", message)
	}
	awaitRunnerChange(t, f.a, func() bool { cols, rows := f.a.Size(); return cols == 82 && rows == 22 })
	if want := []string{"\x1b[200~first\x1b[201~", "\r", "raw", "\x1b[200~second\x1b[201~", "\r"}; !reflect.DeepEqual(f.a.Inputs(), want) {
		t.Fatalf("wire order=%q", f.a.Inputs())
	}
}

func TestMuxCloseCancelsPendingConfirmationAndDiscardsLaterInput(t *testing.T) {
	f := newMuxWorkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "one", "data": "pending"})
	awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 2 })
	writeWS(t, ctx, f.socket, map[string]any{"type": "input", "sessionId": "legacy-a", "data": "must not be typed"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "two", "data": "must not be sent"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatalf("pending commands blocked close preparation: %v", message)
	}
	_ = f.socket.CloseNow()
	select {
	case <-f.done:
	case <-time.After(700 * time.Millisecond):
		t.Fatal("connection shutdown waited for delivery timeout")
	}
	if len(f.a.Inputs()) != 2 || f.handler.submits.tracked() != 0 {
		t.Fatalf("late input or retained session lock: %q", f.a.Inputs())
	}
}

func TestMuxCloseCancelsWaitForAnotherTransportSessionLock(t *testing.T) {
	f := newMuxWorkFixture(t)
	unlock := f.handler.submits.lock("legacy-a")
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "waiting", "data": "must not be typed"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatal("mutex waiter blocked read loop")
	}
	waitSessionLockUsers(t, f.handler.submits, "legacy-a", 2)
	_ = f.socket.CloseNow()
	select {
	case <-f.done:
	case <-time.After(700 * time.Millisecond):
		t.Fatal("shutdown stuck behind another transport's lock")
	}
	if len(f.a.Inputs()) != 0 {
		t.Fatal("canceled waiter typed input")
	}
	unlock()
	if f.handler.submits.tracked() != 0 {
		t.Fatal("canceled waiter leaked its lock reference")
	}
}

func TestMuxOverloadIsRefusedWithoutStallingPingOrReplaying(t *testing.T) {
	f := newMuxWorkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "slow", "data": "pending"})
	awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 2 })
	for range muxWorkMessages - 1 {
		writeWS(t, ctx, f.socket, map[string]any{"type": "input", "sessionId": "legacy-a", "data": "pending raw"})
	}
	for _, kind := range []string{"input", "submit"} {
		writeWS(t, ctx, f.socket, map[string]any{"type": kind, "sessionId": "legacy-a", "requestId": "overflow-" + kind, "data": "not admitted"})
		message := readWS(t, ctx, f.socket)
		if message["requestId"] != "overflow-"+kind || message["ok"] != false || !strings.Contains(fmt.Sprint(message["reason"]), "not sent") {
			t.Fatalf("overload response=%v", message)
		}
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if message := readWS(t, ctx, f.socket); message["type"] != "pong" {
		t.Fatal("overload stalled ping")
	}
	_ = f.socket.CloseNow()
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("overloaded close did not finish")
	}
	if len(f.a.Inputs()) != 2 {
		t.Fatal("overload or canceled queue sent extra input")
	}
}

func TestMuxWorkConcurrentAdmissionAndClose(t *testing.T) {
	q := newMuxWork(context.Background(), func(ctx context.Context, _ clientMessage) { <-ctx.Done() })
	var writers sync.WaitGroup
	for range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 50 {
				q.enqueue(clientMessage{Type: "input", SessionID: "a"})
			}
		}()
	}
	q.close()
	writers.Wait()
	if q.count != 0 || q.bytes != 0 || len(q.sessions) != 0 {
		t.Fatal("concurrent close leaked work")
	}
}

func TestMuxLegacyConfirmationDoesNotBlockOtherSessionOrPing(t *testing.T) {
	f := newMuxWorkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writeWS(t, ctx, f.socket, map[string]any{"type": "submit", "sessionId": "legacy-a", "requestId": "slow", "data": "complete fixture message"})
	awaitRunnerChange(t, f.a, func() bool { return len(f.a.Inputs()) == 2 })
	started := time.Now()
	writeWS(t, ctx, f.socket, map[string]any{"type": "input", "sessionId": "terminal-b", "requestId": "fast", "data": "raw-b"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "resize", "sessionId": "terminal-b", "cols": 81, "rows": 21})
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	deadline, stop := context.WithTimeout(ctx, 700*time.Millisecond)
	defer stop()
	gotInput, gotPong := false, false
	for !gotInput || !gotPong {
		message := readWS(t, deadline, f.socket)
		if message["requestId"] == "slow" {
			t.Fatalf("unobserved legacy message acknowledged early: %v", message)
		}
		gotInput = gotInput || (message["requestId"] == "fast" && message["ok"] == true)
		gotPong = gotPong || message["type"] == "pong"
	}
	awaitRunnerChange(t, f.b, func() bool { cols, rows := f.b.Size(); return cols == 81 && rows == 21 && len(f.b.Inputs()) == 1 })
	t.Logf("other-session input/resize and pong while legacy confirmation pending: %s", time.Since(started))
	recordUserEvent(t, f.legacy, time.Now(), "complete fixture message")
	if message := readWS(t, ctx, f.socket); message["requestId"] != "slow" || message["ok"] != true {
		t.Fatalf("completion=%v", message)
	}
}
