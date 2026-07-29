package main

import (
	"bytes"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type durabilityProcess struct {
	output *bytes.Reader
}

func (p *durabilityProcess) Read(buffer []byte) (int, error) {
	return p.output.Read(buffer)
}

func (*durabilityProcess) Write(buffer []byte) (int, error) {
	return len(buffer), nil
}

func (*durabilityProcess) PID() int              { return 42 }
func (*durabilityProcess) Resize(int, int) error { return nil }
func (*durabilityProcess) RequestStop() error    { return nil }
func (*durabilityProcess) ForceKill() error      { return nil }
func (*durabilityProcess) CloseOutput() error    { return nil }
func (*durabilityProcess) Wait(bool) exitInfo    { code := 0; return exitInfo{Code: &code} }

type durabilityHistory struct {
	mu          sync.Mutex
	calls       []string
	syncStarted chan struct{}
	allowSync   chan struct{}
	syncOnce    sync.Once
}

func (h *durabilityHistory) Append(_ uint32, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "append:"+string(data))
	return nil
}

func (h *durabilityHistory) Sync() error {
	h.mu.Lock()
	h.calls = append(h.calls, "sync")
	h.mu.Unlock()
	h.syncOnce.Do(func() { close(h.syncStarted) })
	<-h.allowSync
	return nil
}

func (*durabilityHistory) Close() error  { return nil }
func (*durabilityHistory) Unlink() error { return nil }

func (h *durabilityHistory) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func TestWaitChildSyncsLastEventBeforeExit(t *testing.T) {
	const marker = "LAST_EVENT_BEFORE_EXIT"
	history := &durabilityHistory{
		syncStarted: make(chan struct{}),
		allowSync:   make(chan struct{}),
	}
	syncReleased := false
	defer func() {
		if !syncReleased {
			close(history.allowSync)
		}
	}()
	r := &runner{
		process:    &durabilityProcess{output: bytes.NewReader([]byte(marker))},
		log:        state.NewEventLog(state.DefaultEventCap),
		persistent: history,
		logger:     log.New(io.Discard, "", 0),
		clients:    make(map[*client]struct{}),
		readDone:   make(chan struct{}),
	}

	go r.readOutput()
	done := make(chan struct{})
	go func() {
		r.waitChild()
		close(done)
	}()

	select {
	case <-history.syncStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("waitChild did not reach the durability boundary")
	}
	r.mu.Lock()
	exitBeforeSync := r.exit
	r.mu.Unlock()
	if exitBeforeSync != nil {
		t.Fatalf("EXIT was published before final-event sync completed: %#v", exitBeforeSync)
	}
	close(history.allowSync)
	syncReleased = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitChild did not finish after final-event sync")
	}
	t.Cleanup(func() {
		r.mu.Lock()
		if r.idle != nil {
			r.idle.Stop()
		}
		r.mu.Unlock()
	})

	calls := history.snapshot()
	if len(calls) != 2 || calls[0] != "append:"+marker || calls[1] != "sync" {
		t.Fatalf("durability order = %#v, want append of final event then sync", calls)
	}
	r.mu.Lock()
	exit := r.exit
	r.mu.Unlock()
	if exit == nil || exit.Seq != 1 {
		t.Fatalf("published exit = %#v, want durable sequence 1", exit)
	}
}
