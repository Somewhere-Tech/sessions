package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

func TestMuxAttachmentReservationsBoundConcurrentAdmission(t *testing.T) {
	attached := newMuxAttachments()
	var admitted atomic.Int64
	var workers sync.WaitGroup
	for index := range maxMuxAttachments * 2 {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			if slot, _ := attached.reserve(fmt.Sprint(index)); slot != nil {
				admitted.Add(1)
			}
		}(index)
	}
	workers.Wait()
	if got := admitted.Load(); got != maxMuxAttachments {
		t.Fatalf("admitted=%d, want %d", got, maxMuxAttachments)
	}
	for id := range attached.entries {
		if slot, full := attached.reserve(id); slot != nil || full {
			t.Fatal("duplicate pending attachment consumed capacity")
		}
		attached.detach(id)
		if slot, full := attached.reserve("replacement"); slot == nil || full {
			t.Fatal("detaching a pending reservation did not release capacity")
		}
		break
	}
	attached.close()
	if slot, full := attached.reserve("closed"); slot != nil || full {
		t.Fatal("closed connection admitted an attachment")
	}
}

func TestMuxAttachmentCancellationDoesNotRemoveAReplacement(t *testing.T) {
	attached := newMuxAttachments()
	old, _ := attached.reserve("session")
	attached.detach("session")
	replacement, _ := attached.reserve("session")
	var cancelled atomic.Int64
	if attached.install("session", old, func() { cancelled.Add(1) }) {
		t.Fatal("stale replay installed into a replacement reservation")
	}
	attached.release("session", old)
	if slot, full := attached.reserve("session"); slot != nil || full {
		t.Fatal("stale stream cleanup removed a replacement")
	}
	if !attached.install("session", replacement, func() { cancelled.Add(1) }) {
		t.Fatal("replacement could not install")
	}
	attached.close()
	attached.close()
	if got := cancelled.Load(); got != 2 {
		t.Fatalf("cancelled=%d, want 2", got)
	}
}

func TestMuxAttachmentLimitKeepsSocketAndSessionsAlive(t *testing.T) {
	f := newMuxWorkFixture(t, func(f *muxWorkFixture) {
		for index := range maxMuxAttachments + 1 {
			id := fmt.Sprintf("stream-%d", index)
			runner := prototest.NewRunner(proto.RunnerInfo{ID: id, Cmd: "/bin/sh", Cwd: f.root, Cols: 120, Rows: 40, ProtocolVersion: 2})
			session, err := f.registry.Register(context.Background(), runner, id, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index := range maxMuxAttachments {
		id := fmt.Sprintf("stream-%d", index)
		writeWS(t, ctx, f.socket, map[string]any{"type": "attach", "sessionId": id, "outputReplay": false, "claudeReplay": false})
		if hello := readWS(t, ctx, f.socket); hello["type"] != "hello" || hello["sessionId"] != id {
			t.Fatalf("attach %d = %#v", index, hello)
		}
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "attach", "sessionId": "stream-0"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "attach", "sessionId": fmt.Sprintf("stream-%d", maxMuxAttachments)})
	refusal := readWS(t, ctx, f.socket)
	if refusal["type"] != "error" || refusal["code"] != "mux_attachment_limit" {
		t.Fatalf("overflow=%#v", refusal)
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "ping"})
	if reply := readWS(t, ctx, f.socket); reply["type"] != "pong" {
		t.Fatalf("reply=%#v", reply)
	}
	if session, ok := f.registry.Get("stream-0"); !ok || session.Info().Exited {
		t.Fatal("overflow ended an existing session")
	}
	writeWS(t, ctx, f.socket, map[string]any{"type": "detach", "sessionId": "stream-0"})
	writeWS(t, ctx, f.socket, map[string]any{"type": "attach", "sessionId": fmt.Sprintf("stream-%d", maxMuxAttachments), "outputReplay": false, "claudeReplay": false})
	if hello := readWS(t, ctx, f.socket); hello["type"] != "hello" {
		t.Fatalf("replacement attach=%#v", hello)
	}
}
