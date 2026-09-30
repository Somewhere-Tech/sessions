package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// One request, one lane per installed provider, one join: the fan-out is the
// way a change gets checked by an agent from each provider.
func TestFanoutStartsOneLanePerProviderAndJoinsThem(t *testing.T) {
	// This case exercises fanout from a manager, where Claude children use the
	// structured runtime and receive their first request through submit. Do not
	// let that contract depend on whether the test process itself runs inside a
	// real Sessions session.
	t.Setenv("SESSIONS_SESSION_ID", "bbbbbbbb-0000-4000-8000-000000000000")
	t.Setenv("SESSIONS_OWNER_ID", "")

	var mu sync.Mutex
	created := []map[string]any{}
	submitted := map[string]string{}
	ids := map[string]string{"claude": "aaaaaaaa-0000-4000-8000-000000000001", "codex": "aaaaaaaa-0000-4000-8000-000000000002"}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/providers":
			_ = json.NewEncoder(response).Encode(map[string]any{"providers": []map[string]any{
				{"id": "claude", "name": "Claude Code", "installed": true},
				{"id": "codex", "name": "Codex", "installed": true},
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			created = append(created, body)
			tool := "codex"
			if cmd, _ := body["cmd"].(string); strings.Contains(cmd, "claude") {
				tool = "claude"
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{"id": ids[tool], "name": body["name"], "cmd": body["cmd"]})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/submit"):
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/sessions/"), "/submit")
			submitted[id], _ = body["data"].(string)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"operation_id": body["operation_id"], "session_id": id, "status": "accepted", "delivered": true, "retry": false,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			sessions := []map[string]any{}
			for tool, id := range ids {
				sessions = append(sessions, map[string]any{
					"id": id, "name": "check (" + tool + ")", "tool": tool, "working": false, "exited": false,
					"idleReason": "completed", "idleSince": 1, "lastSummary": tool + " found nothing wrong",
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": sessions})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "fanout", "--idle", "10ms", "--timeout", "10s", "--", "review", "the", "diff"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report fanoutReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report %q: %v", stdout.String(), err)
	}
	if !report.OK || len(report.Lanes) != 2 || report.Lanes[0].Provider != "claude" || report.Lanes[1].Provider != "codex" {
		t.Fatalf("report = %+v", report)
	}
	for _, lane := range report.Lanes {
		if lane.ID != ids[lane.Provider] || lane.Outcome == nil || !lane.Outcome.OK || !strings.Contains(lane.Outcome.Summary, "nothing wrong") {
			t.Fatalf("lane = %+v", lane)
		}
		if !strings.Contains(lane.Name, "("+lane.Provider+")") {
			t.Fatalf("lane name = %q", lane.Name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("created = %+v", created)
	}
	// Rich Claude takes the request through submit; a Codex terminal session
	// takes it as its start argument. Either way every lane got it.
	if submitted[ids["claude"]] != "review the diff" {
		t.Fatalf("claude submitted = %q", submitted[ids["claude"]])
	}
	codexGotRequest := submitted[ids["codex"]] == "review the diff"
	for _, body := range created {
		if cmd, _ := body["cmd"].(string); strings.Contains(cmd, "codex") {
			if args, ok := body["args"].([]any); ok {
				for _, arg := range args {
					if text, _ := arg.(string); strings.Contains(text, "review the diff") {
						codexGotRequest = true
					}
				}
			}
		}
	}
	if !codexGotRequest {
		t.Fatalf("codex never received the request: created=%+v submitted=%+v", created, submitted)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "fanout", "--with", "cursor", "--", "x"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown provider exit=%d stderr=%q", code, stderr.String())
	}
}

func TestFanoutNameDoesNotSplitUnicodeAtByteLimit(t *testing.T) {
	request := strings.Repeat("a", 46) + "\U0001F600 follow-up"
	got := fanoutName(request)
	if !utf8.ValidString(got) {
		t.Fatalf("fanoutName() returned invalid UTF-8: %q", got)
	}
	if len(got) > 48 {
		t.Fatalf("fanoutName() length = %d bytes, want at most 48: %q", len(got), got)
	}
	if want := strings.Repeat("a", 46); got != want {
		t.Fatalf("fanoutName() = %q, want %q", got, want)
	}
}

func TestFanoutProviderListDeduplicatesExplicitProviders(t *testing.T) {
	providers, err := (&app{}).fanoutProviderList("claude, CLAUDE, codex,claude")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "codex"}; !reflect.DeepEqual(providers, want) {
		t.Fatalf("providers = %v, want %v", providers, want)
	}
}

func TestFanoutStartsOneLaneForRepeatedExplicitProvider(t *testing.T) {
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost && request.URL.Path == "/api/sessions" {
			created++
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"id":"aaaaaaaa-0000-4000-8000-000000000001","name":"review (codex)","cmd":"codex"}`))
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "fanout", "--with", "codex,codex", "--no-wait", "--", "review"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report fanoutReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if created != 1 || len(report.Lanes) != 1 || report.Lanes[0].Provider != "codex" {
		t.Fatalf("created=%d report=%+v", created, report)
	}
}

func TestFanoutProviderDiscoveryOnlyFallsBackForMissingRoute(t *testing.T) {
	t.Run("missing route", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer application.close()
		providers, err := application.fanoutProviderList("")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(providers, fanoutProviders) {
			t.Fatalf("providers = %v, want legacy fallback %v", providers, fanoutProviders)
		}
	})

	t.Run("authorization failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = response.Write([]byte(`{"error":"provider discovery is not authorized"}`))
		}))
		defer server.Close()
		application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer application.close()
		providers, err := application.fanoutProviderList("")
		if err == nil || len(providers) != 0 || !strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("providers=%v err=%v", providers, err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		application, err := newApp([]string{"--host", url}, strings.NewReader(""), io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer application.close()
		providers, err := application.fanoutProviderList("")
		if err == nil || len(providers) != 0 || !strings.Contains(err.Error(), "could not ask sessionsd") {
			t.Fatalf("providers=%v err=%v", providers, err)
		}
	})
}
