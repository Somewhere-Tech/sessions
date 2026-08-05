package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type stubHistory struct{}

func (*stubHistory) Append(uint32, []byte) error { return nil }
func (*stubHistory) Sync() error                 { return nil }
func (*stubHistory) Close() error                { return nil }
func (*stubHistory) Unlink() error               { return nil }

func (r *runner) idleTimer() *time.Timer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idle
}

func TestReconnectingClientCancelsThePostExitGracePeriod(t *testing.T) {
	code := 0
	r := &runner{
		process:    &durabilityProcess{output: bytes.NewReader(nil)},
		log:        state.NewEventLog(state.DefaultEventCap),
		persistent: &stubHistory{},
		logger:     log.New(io.Discard, "", 0),
		clients:    make(map[*client]struct{}),
		readDone:   make(chan struct{}),
		exited:     true,
		exit:       &exitInfo{Code: &code},
	}
	t.Cleanup(func() {
		r.mu.Lock()
		r.cancelIdleShutdownLocked()
		r.mu.Unlock()
	})

	r.scheduleIdleShutdown()
	if r.idleTimer() == nil {
		t.Fatal("an exited runner with no clients did not arm the grace period")
	}

	server, daemon := net.Pipe()
	go r.serveClient(server)
	// The greeting and the retained EXIT prove the connection is registered.
	for range 2 {
		if _, err := proto.Read(daemon); err != nil {
			t.Fatalf("reconnecting daemon could not read the runner greeting: %v", err)
		}
	}
	if timer := r.idleTimer(); timer != nil {
		t.Fatal("the grace period is still armed while a reconnected client is replaying")
	}

	// Disconnecting starts a fresh grace period rather than leaving the runner
	// alive forever.
	_ = daemon.Close()
	deadline := time.Now().Add(5 * time.Second)
	for r.idleTimer() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the grace period was not re-armed after the client disconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
