package proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// The synchronous answer belongs to this caller and nothing else.
	if _, ok := runner.LateMessageResult(want.OperationID); ok {
		t.Fatal("a delivered answer was also retained as late evidence")
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

// The runner starts the turn before it answers, so an acknowledgment that
// arrives after the caller gave up is still the runner's own correlated
// statement that the message was accepted. Dropping it turned a delivered
// message into permanent uncertainty.
func TestSocketRunnerKeepsAcknowledgementThatArrivesAfterTheCallerGaveUp(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	runner := messageRunner(client, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := runner.SubmitMessage(ctx, MessageControl{OperationID: "late-accept", Text: "ship it"})
		errCh <- err
	}()
	if _, err := Read(server); err != nil {
		t.Fatalf("read request: %v", err)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("SubmitMessage() error = %v, want context.Canceled", err)
	}
	if _, ok := runner.LateMessageResult("late-accept"); ok {
		t.Fatal("a result was reported before the runner answered")
	}

	payload, _ := json.Marshal(MessageResult{OperationID: "late-accept", Accepted: true, Boundary: "provider"})
	if err := Write(server, MessageRes, payload); err != nil {
		t.Fatalf("write late acknowledgement: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		result, ok := runner.LateMessageResult("late-accept")
		if ok {
			if !result.Accepted || result.Boundary != "provider" {
				t.Fatalf("late result = %+v", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's late acknowledgement was dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Only unclaimed answers are retained: an operation nobody submitted has no
	// evidence, and a delivered answer must not be invented for it.
	if _, ok := runner.LateMessageResult("never-sent"); ok {
		t.Fatal("an unrelated operation id produced evidence")
	}
}

// The narrow interleaving the first repair missed: the reader wins the mutex
// while the waiter is still registered and queues the answer into the buffered
// channel, but the caller's select has already chosen to give up. Driving the
// two halves in that order is deterministic and needs no sleep: cancellation
// and the acknowledgment timeout both retire the waiter through this one
// cleanup, so covering it covers both.
func TestAbandonedWaiterKeepsAnAnswerThatWasAlreadyQueued(t *testing.T) {
	runner := messageRunner(nil, true)
	response := make(chan MessageResult, 1)
	runner.mu.Lock()
	runner.messages = map[string]chan MessageResult{"racing": response}
	runner.mu.Unlock()

	payload, _ := json.Marshal(MessageResult{OperationID: "racing", Accepted: true, Boundary: "runner"})
	runner.handleMessageResponse(payload)
	runner.finishMessageWaiter("racing", response)

	result, ok := runner.LateMessageResult("racing")
	if !ok || !result.Accepted || result.Boundary != "runner" {
		t.Fatalf("late result = %+v, ok = %t; a queued answer was dropped by cleanup", result, ok)
	}
	runner.mu.Lock()
	_, waiting := runner.messages["racing"]
	runner.mu.Unlock()
	if waiting {
		t.Fatal("cleanup left the waiter registered")
	}
}

// A caller that received its answer owns it. Cleanup must not also file it as
// late evidence, and an operation that was never answered must not acquire one.
func TestFinishedWaiterRetainsNothingWhenTheCallerWasAnswered(t *testing.T) {
	runner := messageRunner(nil, true)
	answered := make(chan MessageResult, 1)
	answered <- MessageResult{OperationID: "answered", Accepted: true, Boundary: "runner"}
	<-answered // the caller's select consumed it, exactly as SubmitMessage does
	runner.finishMessageWaiter("answered", answered)
	if _, ok := runner.LateMessageResult("answered"); ok {
		t.Fatal("a consumed answer was also filed as late evidence")
	}

	runner.finishMessageWaiter("silent", make(chan MessageResult, 1))
	if _, ok := runner.LateMessageResult("silent"); ok {
		t.Fatal("an unanswered operation acquired evidence")
	}
}

// A connection that drops after the answer was queued still knows the answer.
func TestClosedConnectionKeepsAQueuedAnswerAndInventsNothing(t *testing.T) {
	runner := messageRunner(nil, true)
	queued := make(chan MessageResult, 1)
	queued <- MessageResult{OperationID: "queued", Accepted: true, Boundary: "provider"}
	close(queued)
	runner.finishMessageWaiter("queued", queued)
	if result, ok := runner.LateMessageResult("queued"); !ok || result.Boundary != "provider" {
		t.Fatalf("late result after close = %+v, ok = %t", result, ok)
	}

	closedEmpty := make(chan MessageResult)
	close(closedEmpty)
	runner.finishMessageWaiter("lost", closedEmpty)
	if _, ok := runner.LateMessageResult("lost"); ok {
		t.Fatal("a closed empty channel produced an acknowledgement")
	}
}

// The same race through the real submit path, asserted without depending on
// which branch the scheduler picks. Reading the request frame from an unbuffered
// pipe proves the waiter is registered and the write has returned, so queueing
// the answer and cancelling there makes both select cases ready: Go may take
// either. Whichever it takes, the answer must survive — returned to this caller
// or retained as late evidence — and it must never be lost.
func TestSubmitMessageNeverLosesAnAnswerRacingItsCancellation(t *testing.T) {
	for attempt := range 200 {
		client, server := net.Pipe()
		runner := messageRunner(client, true)
		operationID := fmt.Sprintf("racing-%d", attempt)
		ctx, cancel := context.WithCancel(context.Background())

		type outcome struct {
			result MessageResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := runner.SubmitMessage(ctx, MessageControl{OperationID: operationID, Text: "ship it"})
			done <- outcome{result, err}
		}()
		if _, err := Read(server); err != nil {
			t.Fatalf("read request: %v", err)
		}
		payload, _ := json.Marshal(MessageResult{OperationID: operationID, Accepted: true, Boundary: "provider"})
		runner.handleMessageResponse(payload)
		cancel()

		got := <-done
		late, retained := runner.LateMessageResult(operationID)
		switch {
		case got.err == nil:
			if !got.result.Accepted || got.result.Boundary != "provider" {
				t.Fatalf("returned result = %+v", got.result)
			}
			if retained {
				t.Fatalf("attempt %d: a result was returned and also retained: %+v", attempt, late)
			}
		case errors.Is(got.err, context.Canceled):
			if !retained || !late.Accepted || late.Boundary != "provider" {
				t.Fatalf("attempt %d: cancellation lost an answer that was already queued", attempt)
			}
		default:
			t.Fatalf("attempt %d: SubmitMessage() error = %v", attempt, got.err)
		}
		cancel()
		_ = client.Close()
		_ = server.Close()
	}
}

// The retention bound is what keeps an abandoned-operation cache from becoming
// a leak, so it is pinned rather than assumed. Deterministic: every answer is
// handed to the same reader entry point with no waiter registered.
func TestRetainedAcknowledgementsStayBoundedAndDropTheOldestFirst(t *testing.T) {
	runner := messageRunner(nil, true)
	answer := func(operationID string, boundary string) {
		payload, err := json.Marshal(MessageResult{OperationID: operationID, Accepted: true, Boundary: boundary})
		if err != nil {
			t.Fatal(err)
		}
		runner.handleMessageResponse(payload)
	}

	const overflow = 10
	for index := range lateMessageResultLimit + overflow {
		answer(fmt.Sprintf("operation-%03d", index), "runner")
	}
	runner.mu.Lock()
	kept, order := len(runner.lateMessages), len(runner.lateOrder)
	oldest := runner.lateOrder[0]
	runner.mu.Unlock()
	if kept != lateMessageResultLimit || order != lateMessageResultLimit {
		t.Fatalf("retained %d results and %d order entries, want %d of each", kept, order, lateMessageResultLimit)
	}
	if want := fmt.Sprintf("operation-%03d", overflow); oldest != want {
		t.Fatalf("oldest retained operation = %q, want %q", oldest, want)
	}
	for index := range overflow {
		if _, ok := runner.LateMessageResult(fmt.Sprintf("operation-%03d", index)); ok {
			t.Fatalf("operation-%03d survived past the bound", index)
		}
	}
	newest := fmt.Sprintf("operation-%03d", lateMessageResultLimit+overflow-1)
	if _, ok := runner.LateMessageResult(newest); !ok {
		t.Fatalf("%s was not retained", newest)
	}

	// A runner that answers the same operation again replaces the value in
	// place: re-recording an id must not queue it for eviction twice or push a
	// live operation out early.
	for range 5 {
		answer(newest, "provider")
	}
	runner.mu.Lock()
	kept, order = len(runner.lateMessages), len(runner.lateOrder)
	stillOldest := runner.lateOrder[0]
	runner.mu.Unlock()
	if kept != lateMessageResultLimit || order != lateMessageResultLimit {
		t.Fatalf("repeated answers grew storage to %d results and %d order entries", kept, order)
	}
	if stillOldest != oldest {
		t.Fatalf("repeated answers evicted %q, oldest is now %q", oldest, stillOldest)
	}
	if result, ok := runner.LateMessageResult(newest); !ok || result.Boundary != "provider" {
		t.Fatalf("latest answer for %s = %+v, ok = %t", newest, result, ok)
	}
}
