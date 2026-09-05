package proto

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func messageRunner(conn net.Conn, capable bool) *SocketRunner {
	return &SocketRunner{
		conn: conn,
		info: RunnerInfo{ProtocolVersion: ProtocolVersion, MessageSubmit: capable},
		subs: make(map[uint64]chan Event),
	}
}

func TestSocketRunnerSubmitMessageWritesOneStructuredFrame(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	runner := messageRunner(client, true)
	want := MessageControl{OperationID: "message-1", Text: "first line\rsecond \x1b[31mline", Mode: "steer"}

	serverErr := make(chan error, 1)
	go func() {
		frame, err := Read(server)
		if err != nil {
			serverErr <- err
			return
		}
		if frame.Type != MessageReq {
			serverErr <- errors.New("expected one message request frame")
			return
		}
		var got MessageControl
		if err := json.Unmarshal(frame.Payload, &got); err != nil {
			serverErr <- err
			return
		}
		if got != want {
			serverErr <- errors.New("message control changed across the wire")
			return
		}
		payload, _ := json.Marshal(MessageResult{OperationID: want.OperationID, Accepted: true, Boundary: "runner"})
		serverErr <- Write(server, MessageRes, payload)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runner.SubmitMessage(ctx, want)
	if err != nil {
		t.Fatalf("SubmitMessage() error = %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("serve message: %v", err)
	}
	if !result.Accepted || result.OperationID != want.OperationID || result.Boundary != "runner" {
		t.Fatalf("SubmitMessage() result = %+v", result)
	}
}

func TestSocketRunnerSubmitMessageCorrelatesAcknowledgement(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	runner := messageRunner(client, true)
	result := make(chan MessageResult, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := runner.SubmitMessage(context.Background(), MessageControl{OperationID: "ours", Text: "hello"})
		result <- got
		errs <- err
	}()
	if _, err := Read(server); err != nil {
		t.Fatalf("read request: %v", err)
	}
	wrong, _ := json.Marshal(MessageResult{OperationID: "theirs", Accepted: true})
	if err := Write(server, MessageRes, wrong); err != nil {
		t.Fatalf("write unrelated response: %v", err)
	}
	select {
	case got := <-result:
		t.Fatalf("unrelated acknowledgement completed request: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}
	right, _ := json.Marshal(MessageResult{OperationID: "ours", Accepted: true})
	if err := Write(server, MessageRes, right); err != nil {
		t.Fatalf("write correlated response: %v", err)
	}
	if got := <-result; !got.Accepted || got.OperationID != "ours" {
		t.Fatalf("correlated result = %+v", got)
	}
	if err := <-errs; err != nil {
		t.Fatalf("SubmitMessage() error = %v", err)
	}
}

func TestSocketRunnerSubmitMessageRejectsMissingCapabilityWithoutWriting(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	runner := messageRunner(client, false)

	if _, err := runner.SubmitMessage(context.Background(), MessageControl{OperationID: "unsupported", Text: "hello"}); err == nil {
		t.Fatal("SubmitMessage() succeeded without message capability")
	}
	if err := server.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := Read(server); err == nil {
		t.Fatal("runner wrote a message frame without message capability")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("Read() error = %v, want timeout proving no write", err)
	}
}

func TestSocketRunnerSubmitMessageDoesNotSucceedAfterDisconnectOrCancel(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(net.Conn, context.CancelFunc)
		want error
	}{
		{name: "disconnect", stop: func(server net.Conn, _ context.CancelFunc) { _ = server.Close() }, want: net.ErrClosed},
		{name: "cancel", stop: func(_ net.Conn, cancel context.CancelFunc) { cancel() }, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			runner := messageRunner(client, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				_, err := runner.SubmitMessage(ctx, MessageControl{OperationID: test.name, Text: "hello"})
				errCh <- err
			}()
			if _, err := Read(server); err != nil {
				t.Fatalf("read request: %v", err)
			}
			test.stop(server, cancel)
			select {
			case err := <-errCh:
				if !errors.Is(err, test.want) {
					t.Fatalf("SubmitMessage() error = %v, want %v", err, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("SubmitMessage() did not return after transport stopped")
			}
		})
	}
}
