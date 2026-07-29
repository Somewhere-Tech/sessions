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
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+target+"/input":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			inputs = append(inputs, received{
				data: body["data"], source: request.Header.Get("X-Sessions-Creator-Session"),
			})
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
