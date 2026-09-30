package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendFromAttributesTextButNotEnter(t *testing.T) {
	const target = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const source = "11111111-2222-4333-8444-555555555555"
	type received struct {
		data   string
		source string
	}
	var inputs []received
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{
				map[string]any{"id": target, "name": "target", "tool": "terminal"},
				map[string]any{"id": source, "name": "source", "tool": "terminal"},
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+target+"/submit":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			inputs = append(inputs, received{
				data: body["data"], source: request.Header.Get("X-Sessions-Creator-Session"),
			})
			inputs = append(inputs, received{data: "\r"})
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	application.sleep = func(time.Duration) {}
	if err := application.cmdSend([]string{target, "--from", source, "--no-wait", "review", "this"}); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 {
		t.Fatalf("inputs = %#v", inputs)
	}
	if inputs[0].data != "review this" || inputs[0].source != source {
		t.Fatalf("text input = %#v", inputs[0])
	}
	if inputs[1].data != "\r" || inputs[1].source != "" {
		t.Fatalf("enter input = %#v", inputs[1])
	}
}

func TestSendDoesNotForwardAnInheritedSourceFromAnotherDaemon(t *testing.T) {
	const target = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const foreignSource = "11111111-2222-4333-8444-555555555555"
	const text = "review this scratch result"
	submitted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			lastUser := any(nil)
			if submitted {
				lastUser = int64(2)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": target, "name": "scratch target", "tool": "codex", "lastUserMessageAt": lastUser,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+target+"/events":
			events := []any{}
			if submitted {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": text},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+target+"/submit":
			if got := request.Header.Get("X-Sessions-Creator-Session"); got != "" {
				t.Errorf("foreign creator header = %q, want omitted", got)
			}
			submitted = true
			_ = json.NewEncoder(response).Encode(map[string]any{
				"status": "accepted", "delivered": true,
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", foreignSource)

	var stdout, stderr strings.Builder
	code := run(
		[]string{"--host", server.URL, "send", target, text},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 || stdout.String() != "delivered\n" || stderr.Len() != 0 {
		t.Fatalf("send exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSendKeepsAnInheritedSourceOwnedByTheSelectedDaemon(t *testing.T) {
	const target = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const source = "11111111-2222-4333-8444-555555555555"
	var creator string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{
				map[string]any{"id": target, "tool": "terminal"},
				map[string]any{"id": source, "tool": "terminal"},
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+target+"/submit":
			creator = request.Header.Get("X-Sessions-Creator-Session")
			_ = json.NewEncoder(response).Encode(map[string]any{"status": "accepted", "delivered": true})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", source)

	application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	if err := application.cmdSend([]string{target, "review this"}); err != nil {
		t.Fatal(err)
	}
	if creator != source {
		t.Fatalf("creator header = %q, want %q", creator, source)
	}
}
