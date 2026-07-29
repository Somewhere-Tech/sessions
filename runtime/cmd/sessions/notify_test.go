package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifyCLIStatusAndPerKindToggle(t *testing.T) {
	current := notifyStatus{
		Notify:     notifySettings{Done: true, Waiting: true, Lost: true},
		Subscribed: true,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/notify" {
			http.NotFound(response, request)
			return
		}
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(current)
		case http.MethodPost:
			var body struct {
				Enabled bool   `json:"enabled"`
				Kind    string `json:"kind"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			if body.Kind != "waiting" || body.Enabled {
				t.Errorf("toggle body = %#v", body)
			}
			current.Notify.Waiting = body.Enabled
			_ = json.NewEncoder(response).Encode(current)
		default:
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "notify", "status"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"done: on", "waiting: on", "lost: on", "Push subscription: active."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q: %q", want, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "notify", "off", "waiting"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("toggle exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Notifications disabled for waiting.") || !strings.Contains(stdout.String(), "waiting: off") {
		t.Fatalf("toggle output = %q", stdout.String())
	}
}

func TestNotifyCLITeachesValidKinds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"notify", "off", "lane-death"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "choose done, waiting, or lost") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
