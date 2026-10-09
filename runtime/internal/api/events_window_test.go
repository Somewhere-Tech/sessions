package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// relayAttributionRegistry adds the message-relay ledger the attribution layer
// looks for, which the plain test registry does not implement.
type relayAttributionRegistry struct {
	sessionService
	relays []ledger.MessageRelayed
}

func (r *relayAttributionRegistry) MessageRelays(context.Context, string) ([]ledger.MessageRelayed, error) {
	return append([]ledger.MessageRelayed(nil), r.relays...), nil
}

func recordUserEvent(t *testing.T, session *state.Session, at time.Time, text string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.RecordClaudeEvent(encoded)
}

// TestEventsAuthorshipIsTheSameOverHTTPAndTheWebSocketMux pins the agreement
// between the two transports. The mux path used to return the same events with
// no `author` field at all, so which agent had sent a message depended on which
// transport the reader happened to use.
func TestEventsAuthorshipIsTheSameOverHTTPAndTheWebSocketMux(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("created session was not registered")
	}
	const relayed = "deploy the migration"
	at := time.Now()
	recordUserEvent(t, session, at.Add(-time.Minute), "unrelated earlier question")
	recordUserEvent(t, session, at, relayed)
	daemon.handler.registry = &relayAttributionRegistry{
		sessionService: daemon.registry,
		relays:         []ledger.MessageRelayed{testRelay(at.UnixMilli(), "Release lane", relayed)},
	}

	authorOf := func(source string, events []any) string {
		t.Helper()
		if len(events) != 1 {
			t.Fatalf("%s window = %#v, want exactly the requested event", source, events)
		}
		event, ok := events[0].(map[string]any)
		if !ok {
			t.Fatalf("%s event = %#v", source, events[0])
		}
		author, ok := event["author"].(map[string]any)
		if !ok {
			t.Fatalf("%s event carries no author: %#v", source, event)
		}
		name, _ := author["name"].(string)
		return name
	}

	response := serve(t, daemon.handler, http.MethodGet,
		"/api/sessions/"+info.ID+"/events?tail=1", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", response.Code, response.Body.String())
	}
	var httpBody struct {
		Events     []any `json:"events"`
		StartIndex int64 `json:"startIndex"`
		EndIndex   int64 `json:"endIndex"`
		TotalCount int64 `json:"totalCount"`
	}
	decodeBody(t, response, &httpBody)
	if httpBody.StartIndex != 1 || httpBody.EndIndex != 2 || httpBody.TotalCount != 2 {
		t.Fatalf("http window = %#v", httpBody)
	}
	httpAuthor := authorOf("http", httpBody.Events)

	httpServer := httptest.NewServer(daemon.handler)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mux, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws?mux=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.CloseNow()
	writeWS(t, ctx, mux, map[string]any{
		"type": "events", "requestId": "events-1", "sessionId": info.ID, "tail": 1,
	})
	message := readWS(t, ctx, mux)
	if message["type"] != "events" || message["requestId"] != "events-1" {
		t.Fatalf("mux events = %#v", message)
	}
	muxEvents, _ := message["events"].([]any)
	if muxAuthor := authorOf("mux", muxEvents); muxAuthor != httpAuthor {
		t.Fatalf("mux author = %q, http author = %q", muxAuthor, httpAuthor)
	}
	if httpAuthor != "Release lane" {
		t.Fatalf("author name = %q", httpAuthor)
	}
	if message["startIndex"] != float64(httpBody.StartIndex) || message["endIndex"] != float64(httpBody.EndIndex) {
		t.Fatalf("mux window = %#v, http window = %#v", message, httpBody)
	}

	// The mux path now accepts `before`, so an agent can page backwards on
	// either transport and read the same window.
	writeWS(t, ctx, mux, map[string]any{
		"type": "events", "requestId": "events-2", "sessionId": info.ID, "before": 1, "tail": 1,
	})
	message = readWS(t, ctx, mux)
	older, _ := message["events"].([]any)
	if len(older) != 1 || message["startIndex"] != float64(0) || message["endIndex"] != float64(1) {
		t.Fatalf("mux before-window = %#v", message)
	}
}

// TestEventsPagingCostDoesNotGrowWithHistory is the scaling half. Serving one
// page used to fetch and annotate the ENTIRE history and then slice it, so
// every page of a long conversation cost the whole conversation.
func TestEventsPagingCostDoesNotGrowWithHistory(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/bash", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := daemon.registry.Get(info.ID)
	if !ok {
		t.Fatal("created session was not registered")
	}
	const history = 5000
	at := time.Now().Add(-time.Hour)
	for index := 0; index < history; index++ {
		recordUserEvent(t, session, at.Add(time.Duration(index)*time.Millisecond),
			fmt.Sprintf("history message %d with enough text to be worth parsing", index))
	}

	request := func(target string) func() {
		return func() {
			response := serve(t, daemon.handler, http.MethodGet, target, nil, "127.0.0.1:1", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("events status=%d", response.Code)
			}
		}
	}
	page := allocatedBytesPerCall(request("/api/sessions/" + info.ID + "/events?tail=10"))
	full := allocatedBytesPerCall(request("/api/sessions/" + info.ID + "/events?tail=" + fmt.Sprint(history)))
	if page > full/4 {
		t.Fatalf("a 10-event page allocated %d bytes and the whole %d-event history allocated %d: "+
			"paging still pays for the entire conversation", page, history, full)
	}
	if page > 256<<10 {
		t.Fatalf("a 10-event page allocated %d bytes", page)
	}
}

func allocatedBytesPerCall(do func()) uint64 {
	const repetitions = 20
	do()
	goruntime.GC()
	var before, after goruntime.MemStats
	goruntime.ReadMemStats(&before)
	for index := 0; index < repetitions; index++ {
		do()
	}
	goruntime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / repetitions
}

// An unconfirmed raw-input steer keeps its text in history as
// unconfirmedInput, not as a user event, and names no operation. The agent
// that sent it is still its author there, matched by content and time even
// though its relay is recorded only after the provider call gave up; the event
// is not reshaped into a delivered user message by being attributed.
func TestEventsAttributeAnUnconfirmedSteerToItsSender(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/bash", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := daemon.registry.Get(info.ID)
	const steered = "Ändere den Plan:\n\tzweite Zeile — 日本語 ✓"
	at := time.Now()
	encoded, err := json.Marshal(map[string]any{
		"type": "system", "subtype": "input_rejected", "source": "codex-app-server",
		"timestamp": at.Format(time.RFC3339Nano), "unconfirmed": true,
		"unconfirmedInput": steered, "error": "Codex did not confirm",
	})
	if err != nil {
		t.Fatal(err)
	}
	session.RecordClaudeEvent(encoded)
	daemon.handler.registry = &relayAttributionRegistry{
		sessionService: daemon.registry,
		// The relay is recorded when the runner answers, after a timed-out
		// provider call: later than the submission the record is stamped with.
		relays: []ledger.MessageRelayed{testRelay(at.Add(6*time.Second).UnixMilli(), "Release lane", steered)},
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/events?tail=1", nil, "127.0.0.1:1", nil)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	decodeBody(t, response, &body)
	if len(body.Events) != 1 {
		t.Fatalf("events = %#v", body.Events)
	}
	event := body.Events[0]
	author, _ := event["author"].(map[string]any)
	if author["name"] != "Release lane" || event["type"] != "system" || event["unconfirmedInput"] != steered {
		t.Fatalf("unconfirmed steer = %#v", event)
	}
}

// A record naming its delivery operation is attributed only by that
// operation, not by matching text: an identical message relayed by another
// lane around the same time is not its author, and a person's own unconfirmed
// steer stays unattributed. A raw-input record names no operation, so it still
// matches a relay without one by content and time.
func TestUnconfirmedSteerAuthorshipFollowsItsOperation(t *testing.T) {
	daemon := newTestDaemon(t)
	info, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/bash", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	session, _ := daemon.registry.Get(info.ID)
	const steered, typed = "Bitte erneut prüfen ✓", "Raw input steer ✓"
	at := time.Now()
	record := func(text, operationID string) {
		value := map[string]any{
			"type": "system", "subtype": "input_rejected", "source": "codex-app-server",
			"timestamp": at.Format(time.RFC3339Nano), "unconfirmed": true,
			"unconfirmedInput": text, "error": "Codex did not confirm",
		}
		if operationID != "" {
			value["operationId"] = operationID
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		session.RecordClaudeEvent(encoded)
	}
	record(steered, "op-person")
	record(steered, "op-agent")
	record(typed, "")
	relay := func(offset time.Duration, name, text, operationID string) ledger.MessageRelayed {
		value := testRelay(at.Add(offset).UnixMilli(), name, text)
		value.EventID, value.OperationID = "relay-"+name, operationID
		return value
	}
	daemon.handler.registry = &relayAttributionRegistry{
		sessionService: daemon.registry,
		relays: []ledger.MessageRelayed{
			relay(time.Second, "Unrelated lane", steered, "op-unrelated"),
			relay(2*time.Second, "Operationless lane", steered, ""),
			relay(6*time.Second, "Release lane", steered, "op-agent"),
			relay(7*time.Second, "Raw input lane", typed, ""),
		},
	}
	response := serve(t, daemon.handler, http.MethodGet, "/api/sessions/"+info.ID+"/events?tail=3", nil, "127.0.0.1:1", nil)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	decodeBody(t, response, &body)
	names := make(map[string]any)
	for _, event := range body.Events {
		author, _ := event["author"].(map[string]any)
		operationID, _ := event["operationId"].(string)
		names[operationID] = author["name"]
	}
	if names["op-agent"] != "Release lane" || names[""] != "Raw input lane" || names["op-person"] != nil {
		t.Fatalf("authors by operation = %#v", names)
	}
}
