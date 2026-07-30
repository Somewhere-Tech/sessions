package main

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestClientOutboxPreservesFrameOrder(t *testing.T) {
	runnerSide, daemonSide := net.Pipe()
	c := newClient(runnerSide)
	t.Cleanup(c.close)
	t.Cleanup(func() { _ = daemonSide.Close() })

	first := []byte("first")
	if !c.enqueue(proto.Structured, first) {
		t.Fatal("enqueue first frame failed")
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- c.write(proto.ReplayDone, []byte("second"))
	}()

	for index, want := range []struct {
		typ     proto.Type
		payload []byte
	}{
		{proto.Structured, first},
		{proto.ReplayDone, []byte("second")},
	} {
		frame, err := proto.Read(daemonSide)
		if err != nil {
			t.Fatalf("read frame %d: %v", index, err)
		}
		if frame.Type != want.typ || !bytes.Equal(frame.Payload, want.payload) {
			t.Fatalf("frame %d = %#v, want type %d payload %q", index, frame, want.typ, want.payload)
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("synchronous write = %v", err)
	}
}

func TestClientOutboxDisconnectsSlowReaderWithoutBlockingProducer(t *testing.T) {
	runnerSide, daemonSide := net.Pipe()
	c := newClient(runnerSide)
	t.Cleanup(c.close)
	t.Cleanup(func() { _ = daemonSide.Close() })

	frame := proto.MustEncode(proto.Output, bytes.Repeat([]byte("x"), 32*1024))
	start := time.Now()
	for index := 0; index < clientOutboxFrames+2; index++ {
		c.enqueueFrame(frame)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("slow client blocked producer for %s", elapsed)
	}
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		t.Fatal("slow client was not disconnected when its bounded outbox filled")
	}
}
