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
