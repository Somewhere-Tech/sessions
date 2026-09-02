package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
)

func TestDecideSendConfirmation(t *testing.T) {
	tests := []struct {
		name       string
		evidence   sendEvidence
		confidence string
		exitCode   int
	}{
		{"confirmed", sendEvidence{JSONLConfirmed: true}, "confirmed", 0},
		{"accepted-working", sendEvidence{Working: true}, "accepted", 0},
		{"still-in-composer", sendEvidence{TextStillInComposer: true}, "unconfirmed", 1},
		{"ambiguous", sendEvidence{}, "unconfirmed", 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := decideSendConfirmation(test.evidence)
			if actual.Confidence != test.confidence || actual.ExitCode != test.exitCode {
				t.Fatalf("decision = %#v, want confidence=%s exit=%d", actual, test.confidence, test.exitCode)
			}
			t.Logf("%s: confidence=%s exit=%d", test.name, actual.Confidence, actual.ExitCode)
		})
	}
}

func TestClaudeSubmitSequenceMatchesNodeCLI(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	text := "\x1b[200~Reply with exactly PONG.\x1b[201~"
	inputs := make([]string, 0, 2)
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
				"id": id, "cmd": "claude", "tool": "claude-code", "lastUserMessageAt": lastUser,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			events := []any{}
			if submitted {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": text},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			inputs = append(inputs, body["data"], "\r")
			submitted = true
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
	var sleeps []time.Duration
	application.sleep = func(duration time.Duration) { sleeps = append(sleeps, duration) }
	result, err := application.sendAndConfirm(id, text, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confirmed == nil || !*result.Confirmed || result.Text != text {
		t.Fatalf("result = %+v, want confirmed exact text", result)
	}
	if want := []string{text, "\r"}; !reflect.DeepEqual(inputs, want) {
		t.Fatalf("input sequence = %q, want %q", inputs, want)
	}
	if want := []time.Duration(nil); !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

func TestClaudeNoWaitStillConfirmsProviderDelivery(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const message = "Review the integration findings."
	delivered := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			var lastUser any
			if delivered {
				lastUser = int64(2)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "claude", "tool": "claude-code", "lastUserMessageAt": lastUser,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			events := []any{}
			if delivered {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": message},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			delivered = true
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
	result, err := application.sendAndConfirm(id, message, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confirmed == nil || !*result.Confirmed || result.Text != message {
		t.Fatalf("--no-wait result = %+v, want confirmed provider event", result)
	}
}

func TestExplicitSendOperationIsForwardedAndReturned(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const operationID = "11111111-2222-4333-8444-555555555555"
	const message = "Review the durable receipt."
	delivered := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			var lastUser any
			if delivered {
				lastUser = int64(2)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "claude", "tool": "claude-code", "lastUserMessageAt": lastUser,
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			events := []any{}
			if delivered {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": message},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["operation_id"] != operationID || body["data"] != message {
				t.Errorf("submit body = %#v", body)
			}
			delivered = true
			_ = json.NewEncoder(response).Encode(map[string]any{
				"operation_id": operationID, "session_id": id, "status": "accepted", "delivered": true, "retry": false,
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "send", id, "--operation-id", operationID, message}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("send exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var output sendJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.OperationID != operationID || output.Submitted == nil || !*output.Submitted || output.Confidence != "confirmed" {
		t.Fatalf("send output = %+v", output)
	}
}

func TestUnknownSendReceiptIsMachineReadableAndNotRetried(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const operationID = "11111111-2222-4333-8444-555555555555"
	submits := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "claude", "tool": "claude-code",
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			submits++
			_ = json.NewEncoder(response).Encode(map[string]any{
				"operation_id": operationID, "session_id": id, "status": "unknown", "delivered": false,
				"retry": false, "reason": "connection ended after intent was recorded", "duplicate": true,
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "send", id, "--operation-id", operationID, "do not duplicate"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitTransport || submits != 1 {
		t.Fatalf("send exit=%d submits=%d stderr=%q stdout=%q", code, submits, stderr.String(), stdout.String())
	}
	var output sendJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.OperationID != operationID || output.Submitted == nil || *output.Submitted || output.Confidence != "unknown" {
		t.Fatalf("unknown send output = %+v", output)
	}
}

func TestSendStatusReadsDurableReceipt(t *testing.T) {
	const operationID = "11111111-2222-4333-8444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/message-deliveries/"+operationID {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"operation_id": operationID, "session_id": "target", "status": "accepted", "delivered": true, "retry": false,
		})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "send-status", operationID}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("send-status exit=%d stderr=%q", code, stderr.String())
	}
	var receipt deliveryReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil || receipt.OperationID != operationID || receipt.Status != "accepted" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestLiveStatusNeverLooksTerminalBecauseOfLastTurn(t *testing.T) {
	setAsideAt := int64(1)
	tests := []struct {
		name    string
		session session
		want    string
	}{
		{name: "completed turn remains live", session: session{IdleReason: "completed"}, want: "idle"},
		{name: "failed turn remains live", session: session{IdleReason: "failed"}, want: "idle"},
		{name: "never started remains live", session: session{IdleReason: "never-started"}, want: "idle"},
		{name: "approval is actionable", session: session{IdleReason: "needs-input"}, want: "needs-you"},
		{name: "reboot pause needs recovery", session: session{Unreachable: true, UnreachableReason: "restart-restore-pending"}, want: "needs-recovery"},
		{name: "lost runner is unreachable", session: session{Unreachable: true, UnreachableReason: "runner-lost"}, want: "unreachable"},
		{name: "working wins over last turn", session: session{Working: true, IdleReason: "completed"}, want: "working"},
		{name: "set aside is organizational", session: session{SetAsideAt: &setAsideAt}, want: "set-aside"},
		{name: "only exited is terminal", session: session{Exited: true, IdleReason: "completed"}, want: "exited"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := liveStatusState(test.session); got != test.want {
				t.Fatalf("liveStatusState() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAPIReadFailureMakesRebootPauseActionable(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	err := apiReadFailure("/api/sessions/"+id+"/snapshot", apiResponse{
		status: http.StatusConflict,
		body:   []byte(`{"code":"SESSION_NEEDS_RECREATE","error":"paused after reboot","action":"sessions resume ` + id + `"}`),
	})
	if exitCode(err) != exitTargetUnavailable {
		t.Fatalf("exitCode() = %d, want %d; err=%v", exitCode(err), exitTargetUnavailable, err)
	}
	for _, fragment := range []string{"SESSION_NEEDS_RECREATE", "paused after reboot", "sessions resume " + id} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}

func TestClaudeEnterRetriesRequireTextStillInComposer(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const text = "Reply with exactly PONG."
	tests := []struct {
		name       string
		snapshot   string
		wantEnters int
	}{
		{name: "visible text gets two bounded retries", snapshot: "❯ " + text, wantEnters: 3},
		{name: "cleared composer never retries", snapshot: "❯ ", wantEnters: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enters := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
					_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
						"id": id, "cmd": "claude", "tool": "claude-code", "working": false,
					}}})
				case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/events":
					_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{}, "nextIndex": 0})
				case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/snapshot":
					response.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(response, test.snapshot)
				case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
					var body map[string]string
					_ = json.NewDecoder(request.Body).Decode(&body)
					enters++
					_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
				case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/input":
					enters++
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
			clock := time.Unix(0, 0)
			application.now = func() time.Time { return clock }
			application.sleep = func(duration time.Duration) { clock = clock.Add(duration) }
			result, err := application.sendAndConfirm(id, text, 650*time.Millisecond, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Confirmed == nil || *result.Confirmed {
				t.Fatalf("result = %+v, want unconfirmed timeout", result)
			}
			if enters != test.wantEnters {
				t.Fatalf("Enter count = %d, want %d", enters, test.wantEnters)
			}
		})
	}
}

func TestNodeCLIGoldenOutputShapes(t *testing.T) {
	lsFixture, err := os.ReadFile("testdata/node-ls.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtureSessions []map[string]any
	if err := json.Unmarshal(lsFixture, &fixtureSessions); err != nil {
		t.Fatal(err)
	}
	id, _ := fixtureSessions[0]["id"].(string)
	if id == "" {
		t.Fatal("node ls fixture has no session id")
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			fmt.Fprintf(response, `{"sessions":%s}`, bytes.TrimSpace(lsFixture))
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			_, _ = io.Copy(io.Discard, request.Body)
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		name    string
		args    []string
		fixture string
		exact   bool
	}{
		{"ls", []string{"--host", server.URL, "--json", "ls"}, "testdata/node-ls.json", true},
		{"send", []string{"--host", server.URL, "--json", "send", id[:8], "echo", "GOLDEN_SEND"}, "testdata/node-send.json", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			fixtureBytes, err := os.ReadFile(test.fixture)
			if err != nil {
				t.Fatal(err)
			}
			if test.exact && !bytes.Equal(stdout.Bytes(), fixtureBytes) {
				t.Fatalf("exact Node golden mismatch\nactual:\n%s\nexpected:\n%s", stdout.String(), fixtureBytes)
			}
			var actual, fixture any
			if err := json.Unmarshal(stdout.Bytes(), &actual); err != nil {
				t.Fatalf("decode actual: %v\n%s", err, stdout.String())
			}
			if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if diff := compareJSONShape("$", actual, fixture); diff != "" {
				t.Fatal(diff)
			}
			t.Logf("%s shape matches %s", test.name, test.fixture)
		})
	}
}

func TestNewNameFlowsThroughPostBodyAndList(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var posted createSessionRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions":
			if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"id": id, "name": posted.Name, "cmd": posted.Cmd, "args": posted.Args,
				"cwd": posted.Cwd, "cols": 300, "rows": 50, "createdAt": 1, "pid": 4242,
				"tool": "terminal", "working": false, "lastDataAt": 1,
				"lastUserMessageAt": nil, "exited": false, "exitCode": nil, "exitSignal": nil, "exitedAt": nil,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "name": posted.Name, "cmd": posted.Cmd, "args": posted.Args,
				"cwd": posted.Cwd, "cols": 300, "rows": 50, "createdAt": 1, "pid": 4242,
				"tool": "terminal", "working": false, "lastDataAt": 1,
				"lastUserMessageAt": nil, "exited": false, "exitCode": nil, "exitSignal": nil, "exitedAt": nil,
			}}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "new", "--cmd", "/bin/sh", "--cwd", home, "--name", "  soak label  "}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("new exit=%d stderr=%q", code, stderr.String())
	}
	if posted.Name != "soak label" {
		t.Fatalf("POST name = %q, want soak label", posted.Name)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "--json", "ls"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ls exit=%d stderr=%q", code, stderr.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0]["name"] != "soak label" {
		t.Fatalf("listed sessions = %#v", listed)
	}
}

func TestTagsCommandReadsMergesAndClearsTags(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tags := map[string]string{"product": "sessions"}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{"id": id, "name": "native"}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions/"+id+"/tags":
			_ = json.NewEncoder(response).Encode(map[string]any{"tags": tags})
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+id+"/tags":
			var body tagsResponse
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			tags = body.Tags
			_ = json.NewEncoder(response).Encode(body)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "tags", id[:8], "Team= Core ", "--remove", "product"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("tags exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "team=Core\n" || tags["team"] != "Core" || len(tags) != 1 {
		t.Fatalf("tags output=%q state=%#v", stdout.String(), tags)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "tags", id, "--clear"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("tags --clear exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "(no tags)\n" || len(tags) != 0 {
		t.Fatalf("clear output=%q state=%#v", stdout.String(), tags)
	}
}

func TestRenameCommandUsesDurableNameEndpoint(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var renamed string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{"id": id, "name": "old"}}})
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+id+"/name":
			var body renameResponse
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			renamed = body.Name
			_ = json.NewEncoder(response).Encode(body)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "rename", id[:8], "Database", "work"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("rename exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if renamed != "Database work" || stdout.String() != "Database work\n" {
		t.Fatalf("rename state=%q output=%q", renamed, stdout.String())
	}
}

func TestDescriptionAndDescFlagsFlowThroughNewAndRun(t *testing.T) {
	tests := []struct {
		name string
		path string
		args []string
	}{
		{name: "new description", path: "/api/sessions", args: []string{"new", "--cmd", "/bin/sh", "--description", "  Diagnose release failures  "}},
		{name: "new desc alias", path: "/api/sessions", args: []string{"new", "--cmd", "/bin/sh", "--desc", "  Diagnose release failures  "}},
		{name: "run description", path: "/api/lanes", args: []string{"run", "--description", "  Diagnose release failures  ", "--", "/bin/sh", "--description", "child-value"}},
		{name: "run desc alias", path: "/api/lanes", args: []string{"run", "--desc", "  Diagnose release failures  ", "--", "/bin/sh", "--description", "child-value"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var posted map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != test.path {
					http.NotFound(response, request)
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
					t.Errorf("decode request: %v", err)
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(response, `{"id":"description-test"}`)
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			arguments := append([]string{"--host", server.URL}, test.args...)
			var stdout, stderr bytes.Buffer
			if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if posted["description"] != "Diagnose release failures" {
				t.Fatalf("posted description = %#v", posted)
			}
			if test.path == "/api/lanes" {
				args, _ := posted["args"].([]any)
				if len(args) != 2 || args[0] != "--description" || args[1] != "child-value" {
					t.Fatalf("run child args were changed: %#v", posted["args"])
				}
			}
		})
	}
}

func TestForceThreadsThroughConversationBindingCommands(t *testing.T) {
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	historyID := "provider-history:claude:" + provider
	tests := []struct {
		name string
		args []string
		path string
		body string
	}{
		{name: "new resume", args: []string{"new", "--cmd", "claude", "--resume", provider, "--force"}, path: "/api/sessions", body: `{"id":"new-lane"}`},
		{name: "recover reopen", args: []string{"recover", "--reopen", "--force"}, path: "/api/recovery/reopen", body: `{"ok":true,"outcomes":[]}`},
		{name: "adopt", args: []string{"adopt", provider, "--force"}, path: "/api/recovery/adopt", body: `{"ok":true,"laneId":"adopted-lane","adoption":{}}`},
		{name: "continue exact history", args: []string{"continue", historyID, "--force"}, path: "/api/recovery/adopt", body: `{"ok":true,"laneId":"continued-lane","adoption":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var posted map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != test.path {
					http.NotFound(response, request)
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
					t.Errorf("decode request: %v", err)
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			arguments := append([]string{"--host", server.URL}, test.args...)
			var stdout, stderr bytes.Buffer
			if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if forced, ok := posted["force"].(bool); !ok || !forced {
				t.Fatalf("posted body = %#v, want force=true", posted)
			}
			if test.name == "continue exact history" && posted["historyId"] != historyID {
				t.Fatalf("posted body = %#v, want exact history id %q", posted, historyID)
			}
			if test.name == "continue exact history" {
				if _, present := posted["runtimeMode"]; present {
					t.Fatalf("posted body = %#v, default continuation must let the provider choose its native runtime", posted)
				}
			}
		})
	}
}

func TestContinueWithProviderRequestsCrossProviderContinuation(t *testing.T) {
	historyID := "provider-history:claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/adopt" {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Errorf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, `{"ok":true,"laneId":"continued-lane","sourceHistoryId":"provider-history:claude:source","sourceProvider":"claude","destinationProvider":"codex","mode":"native-import","importedMessages":2}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "continue", historyID, "--with", "codex"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted["historyId"] != historyID || posted["destinationProvider"] != "codex" {
		t.Fatalf("posted body = %#v", posted)
	}
	if stdout.String() != "continued-lane\n" || !strings.Contains(stderr.String(), "source was not modified") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestForkCopiesLiveConversationWithoutEndOrForce(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/fork" {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Errorf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, `{
			"ok":true,
			"laneId":"forked-lane",
			"sourceProvider":"claude",
			"destinationProvider":"codex",
			"mode":"native-import",
			"importedMessages":42,
			"forkedFromSessionId":"source-lane",
			"sourceUntouched":true
		}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "fork", "source-lane", "--with", "codex", "--at", "17", "--message-id", "message-hash"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted["sourceSessionId"] != "source-lane" || posted["destinationProvider"] != "codex" ||
		posted["sourceMessageIndex"] != float64(17) || posted["sourceMessageId"] != "message-hash" {
		t.Fatalf("posted body = %#v", posted)
	}
	if _, forced := posted["force"]; forced {
		t.Fatalf("fork must not force or end its source: %#v", posted)
	}
	if stdout.String() != "forked-lane\n" ||
		!strings.Contains(stderr.String(), "source source-lane keeps running") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestContinueTerminalCanEnableClaudeRemoteControl(t *testing.T) {
	historyID := "provider-history:claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/adopt" {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Errorf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, `{"ok":true,"laneId":"remote-lane","adoption":{}}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "continue", historyID, "--terminal", "--remote-control"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted["historyId"] != historyID || posted["runtimeMode"] != "terminal" || posted["remoteControl"] != true {
		t.Fatalf("posted body = %#v", posted)
	}
	if stdout.String() != "remote-lane\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestContinueStructuredExplicitlyRequestsRichClaude(t *testing.T) {
	historyID := "provider-history:claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/adopt" {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Errorf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, `{"ok":true,"laneId":"structured-lane","adoption":{}}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "continue", historyID, "--structured"},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted["historyId"] != historyID || posted["runtimeMode"] != "rich" {
		t.Fatalf("posted body = %#v", posted)
	}
	if stdout.String() != "structured-lane\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestAdoptPartialSuccessPrintsSafeRepairAndRepairUsesExistingLane(t *testing.T) {
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	laneID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	sourceID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	var posted []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/recovery/adopt" {
			http.NotFound(response, request)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		posted = append(posted, body)
		response.Header().Set("Content-Type", "application/json")
		if body["repairLaneId"] == nil {
			response.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(response, `{
				"ok":false,"partial":true,"laneId":%q,
				"adoption":{"path":%q,"tool":"codex","cwd":"/work","providerUuid":%q,"cmd":"codex","args":["resume",%q]},
				"warning":"the successor is running; repair records only",
				"missingAnnotations":["source-linked"],
				"repair":{"target":%q,"laneId":%q,"sourceSessionId":%q}
			}`, laneID, provider, provider, provider, provider, laneID, sourceID)
			return
		}
		_, _ = fmt.Fprintf(response, `{"ok":true,"laneId":%q,"adoption":{"path":%q}}`, laneID, provider)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run(
		[]string{"--host", server.URL, "adopt", provider},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("partial adopt exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != laneID+"\n" || !strings.Contains(stderr.String(), "repair records only") ||
		!strings.Contains(stderr.String(), "--repair") || !strings.Contains(stderr.String(), laneID) {
		t.Fatalf("partial adopt stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(
		[]string{"--host", server.URL, "adopt", provider, "--repair", laneID, "--source", sourceID},
		strings.NewReader(""), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("repair exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(posted) != 2 || posted[1]["repairLaneId"] != laneID || posted[1]["sourceSessionId"] != sourceID {
		t.Fatalf("repair request bodies=%#v", posted)
	}
}

func compareJSONShape(path string, actual, expected any) string {
	if actual == nil || expected == nil {
		if actual == nil && expected == nil {
			return ""
		}
		return fmt.Sprintf("%s null mismatch: actual=%T expected=%T", path, actual, expected)
	}
	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		return fmt.Sprintf("%s type mismatch: actual=%T expected=%T", path, actual, expected)
	}
	switch expectedValue := expected.(type) {
	case map[string]any:
		actualValue := actual.(map[string]any)
		expectedKeys := make([]string, 0, len(expectedValue))
		actualKeys := make([]string, 0, len(actualValue))
		for key := range expectedValue {
			expectedKeys = append(expectedKeys, key)
		}
		for key := range actualValue {
			actualKeys = append(actualKeys, key)
		}
		sort.Strings(expectedKeys)
		sort.Strings(actualKeys)
		if !reflect.DeepEqual(actualKeys, expectedKeys) {
			return fmt.Sprintf("%s keys mismatch: actual=%v expected=%v", path, actualKeys, expectedKeys)
		}
		for _, key := range expectedKeys {
			if diff := compareJSONShape(path+"."+key, actualValue[key], expectedValue[key]); diff != "" {
				return diff
			}
		}
	case []any:
		actualValue := actual.([]any)
		if len(actualValue) != len(expectedValue) {
			return fmt.Sprintf("%s length mismatch: actual=%d expected=%d", path, len(actualValue), len(expectedValue))
		}
		for index := range expectedValue {
			if diff := compareJSONShape(fmt.Sprintf("%s[%d]", path, index), actualValue[index], expectedValue[index]); diff != "" {
				return diff
			}
		}
	}
	return ""
}

func TestDaemonInstallConfigAndPlistWithoutLaunchctl(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	daemonPath := filepath.Join(binDir, "sessionsd")
	runnerPath := filepath.Join(binDir, "runner")
	for _, path := range []string{daemonPath, runnerPath} {
		if err := os.WriteFile(path, []byte("test binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("SESSIONS_BINARY", daemonPath)
	t.Setenv("SESSIONS_RUNNER", runnerPath)
	t.Setenv("SESSIONS_DAEMON_LABEL", "")

	application, err := newApp([]string{"--host", "127.0.0.1", "--port", "18787"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	config, err := application.daemonInstallConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Label != defaultDaemonLabel {
		t.Fatalf("default label = %q, want %q", config.Label, defaultDaemonLabel)
	}
	if config.DaemonPath != daemonPath || config.RunnerPath != runnerPath {
		t.Fatalf("binary paths = (%q, %q), want (%q, %q)", config.DaemonPath, config.RunnerPath, daemonPath, runnerPath)
	}
	xml := daemonPlist(daemonPlistOptions{
		Label: config.Label, ProgramArguments: []string{config.DaemonPath},
		WorkingDir: filepath.Dir(config.DaemonPath), LogFile: config.LogFile, Env: config.Env,
	})
	for _, want := range []string{
		"<string>" + defaultDaemonLabel + "</string>",
		"<string>" + daemonPath + "</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>SESSIONS_HOST</key>\n    <string>127.0.0.1</string>",
		"<key>SESSIONS_PORT</key>\n    <string>18787</string>",
		"<key>SESSIONS_RUNNER</key>\n    <string>" + runnerPath + "</string>",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("plist missing %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<string>tech.somewhere.sessions.daemon</string>") {
		t.Fatalf("default plist contains the production label:\n%s", xml)
	}
	if err := os.MkdirAll(filepath.Dir(config.PlistPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonPlist(config.PlistPath, xml); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode=%#o, want 0600", info.Mode().Perm())
	}
	t.Logf("default dev plist label=%s mode=%#o (launchctl not invoked)", config.Label, info.Mode().Perm())
}

func TestDaemonLabelIsConfigurableAndValidated(t *testing.T) {
	const scratch = "tech.somewhere.sessions.dev.daemon.scratch-test"
	label, err := resolveDaemonLabel(scratch)
	if err != nil || label != scratch {
		t.Fatalf("resolveDaemonLabel(%q) = %q, %v", scratch, label, err)
	}
	for _, invalid := range []string{"", "/tmp/agent", "bad label", ".hidden"} {
		if invalid == "" {
			continue
		}
		if _, err := resolveDaemonLabel(invalid); err == nil {
			t.Fatalf("resolveDaemonLabel(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestLocateInstallBinaryFindsBuildOutputSuffix(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "sessionsd-"+runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.WriteFile(path, []byte("test binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if got := locateInstallBinary("sessionsd", "", directory); got != path {
		t.Fatalf("locateInstallBinary() = %q, want %q", got, path)
	}
}

func TestInstallRejectsAnOccupiedDaemonPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	clock := time.Unix(0, 0)
	application := &app{
		host: "127.0.0.1", port: port,
		now:   func() time.Time { return clock },
		sleep: func(duration time.Duration) { clock = clock.Add(duration) },
	}
	if err := application.waitForDaemonPortAvailable(200 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "already accepting connections") {
		t.Fatalf("occupied port error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.waitForDaemonPortAvailable(200 * time.Millisecond); err != nil {
		t.Fatalf("released port still reported occupied: %v", err)
	}
}

func TestAgentControlTranslation(t *testing.T) {
	model := "gpt-5.2-codex"
	effort := "high"
	body := createSessionRequest{Cmd: "codex"}
	if err := applyToolDefault(&body, false); err != nil {
		t.Fatal(err)
	}
	if err := applyAgentControls(&body, agentControls{model: &model, effort: &effort, fast: true}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(body.Args, " ")
	for _, want := range []string{
		"--sandbox workspace-write", "--ask-for-approval untrusted", "--model gpt-5.2-codex",
		`model_reasoning_effort="high"`, `service_tier="priority"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
}

func TestCodexNewSelectsStructuredKindWithRevertibleGate(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		args    []string
		kind    string
		wantArg string
	}{
		{name: "safe-default-terminal", wantArg: "--sandbox"},
		{name: "environment-off", env: "0", wantArg: "--sandbox"},
		{name: "flag-off", args: []string{"--pty-codex"}, wantArg: "--sandbox"},
		{name: "full-access-default-rich", args: []string{"--full-access"}, kind: "codex-app-server", wantArg: "--dangerously-bypass-approvals-and-sandbox"},
		{name: "flag-on-overrides-environment", env: "0", args: []string{"--codex-appserver", "--full-access"}, kind: "codex-app-server", wantArg: "--dangerously-bypass-approvals-and-sandbox"},
		{name: "constrained-flag-on-overrides-environment", env: "0", args: []string{"--codex-appserver", "--permissions", "constrained"}, kind: "codex-app-server", wantArg: "untrusted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SESSIONS_CODEX_APPSERVER", test.env)
			var request createSessionRequest
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, httpRequest *http.Request) {
				if httpRequest.URL.Path != "/api/sessions" || httpRequest.Method != http.MethodPost {
					http.NotFound(response, httpRequest)
					return
				}
				if err := json.NewDecoder(httpRequest.Body).Decode(&request); err != nil {
					t.Errorf("decode create request: %v", err)
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = response.Write([]byte(`{"id":"session-1"}`))
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			arguments := []string{"--host", server.URL, "new", "--tool", "codex"}
			arguments = append(arguments, test.args...)
			var stdout, stderr bytes.Buffer
			if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if request.Kind != test.kind {
				t.Fatalf("create kind = %q, want %q", request.Kind, test.kind)
			}
			if !slices.Contains(request.Args, test.wantArg) {
				t.Fatalf("create args = %q, want %q", request.Args, test.wantArg)
			}
		})
	}
}

func TestCodexRichNewSendsPositionalRequestImmediately(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const prompt = "Reply exactly: CODEX_INITIAL_ACCEPTED"
	var request createSessionRequest
	var inputs []string
	submitted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, httpRequest *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case httpRequest.Method == http.MethodPost && httpRequest.URL.Path == "/api/sessions":
			if err := json.NewDecoder(httpRequest.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"id":"` + id + `"}`))
		case httpRequest.Method == http.MethodGet && httpRequest.URL.Path == "/api/sessions":
			lastUser := any(nil)
			if submitted {
				lastUser = int64(2)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "codex", "tool": "codex", "lastUserMessageAt": lastUser,
			}}})
		case httpRequest.Method == http.MethodGet && httpRequest.URL.Path == "/api/sessions/"+id+"/events":
			events := []any{}
			if submitted {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": prompt},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case httpRequest.Method == http.MethodPost && httpRequest.URL.Path == "/api/sessions/"+id+"/submit":
			var body map[string]string
			if err := json.NewDecoder(httpRequest.Body).Decode(&body); err != nil {
				t.Errorf("decode input request: %v", err)
			}
			inputs = append(inputs, body["data"], "\r")
			submitted = true
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(response, httpRequest)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "new", "--tool", "codex", "--full-access", prompt},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if request.Kind != "codex-app-server" {
		t.Fatalf("create kind = %q", request.Kind)
	}
	if slices.Contains(request.Args, prompt) {
		t.Fatalf("positional prompt leaked into app-server args: %q", request.Args)
	}
	if want := []string{prompt, "\r"}; !reflect.DeepEqual(inputs, want) {
		t.Fatalf("input sequence = %q, want %q", inputs, want)
	}
}

func TestCatIncludesApprovalAuditEvents(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{"id": id}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": []any{
				map[string]any{
					"type": "system", "subtype": "approval_requested", "timestamp": "2026-09-02T00:00:00Z",
					"approval": map[string]any{"id": "approval-1", "summary": "Run `touch approved.txt`"},
				},
				map[string]any{
					"type": "system", "subtype": "approval_resolved", "timestamp": "2026-09-02T00:00:01Z",
					"approval": map[string]any{"id": "approval-1", "decision": "allow", "by": "manager-1"},
				},
			}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "cat", id}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("json cat exit=%d stderr=%q", code, stderr.String())
	}
	var turns []messageTurn
	if err := json.Unmarshal(stdout.Bytes(), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Role != "system" || turns[0].Subtype != "approval_requested" ||
		turns[0].Text != "approval_requested: Run `touch approved.txt`" || turns[0].Approval["id"] != "approval-1" ||
		turns[1].Subtype != "approval_resolved" || turns[1].Text != "approval_resolved: allow by manager-1" {
		t.Fatalf("approval turns = %#v", turns)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "cat", id}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("text cat exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"[system]\napproval_requested: Run `touch approved.txt`", "[system]\napproval_resolved: allow by manager-1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("cat output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestClaudeNewUsesStructuredRuntimeForInheritedAgentChildren(t *testing.T) {
	tests := []struct {
		name             string
		args             []string
		creator          string
		kind             string
		fullAccess       bool
		providerTerminal bool
		delegationKind   string
	}{
		{name: "interactive-default"},
		{name: "structured-explicit", args: []string{"--structured"}, kind: "claude-structured"},
		{name: "terminal-explicit", args: []string{"--pty-claude"}, providerTerminal: true},
		{name: "interactive-full-access-explicit", args: []string{"--full-access"}, fullAccess: true},
		{name: "agent-child-default-structured", creator: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", kind: "claude-structured", delegationKind: "agent"},
		{name: "agent-child-terminal-explicit", creator: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", args: []string{"--pty-claude"}, providerTerminal: true, delegationKind: "agent"},
		{name: "detached-external-root-stays-interactive", creator: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", args: []string{"--owner", "external", "--detach"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request createSessionRequest
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, httpRequest *http.Request) {
				if httpRequest.URL.Path != "/api/sessions" || httpRequest.Method != http.MethodPost {
					http.NotFound(response, httpRequest)
					return
				}
				if err := json.NewDecoder(httpRequest.Body).Decode(&request); err != nil {
					t.Errorf("decode create request: %v", err)
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = response.Write([]byte(`{"id":"session-1"}`))
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SESSIONS_SESSION_ID", test.creator)
			t.Setenv("SESSIONS_OWNER_ID", "")
			arguments := []string{"--host", server.URL, "new", "--tool", "claude"}
			arguments = append(arguments, test.args...)
			var stdout, stderr bytes.Buffer
			if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if request.Kind != test.kind {
				t.Fatalf("create kind = %q, want %q", request.Kind, test.kind)
			}
			if request.ProviderTerminal != test.providerTerminal {
				t.Fatalf("providerTerminal = %v, want %v", request.ProviderTerminal, test.providerTerminal)
			}
			if request.DelegationKind != test.delegationKind {
				t.Fatalf("delegationKind = %q, want %q", request.DelegationKind, test.delegationKind)
			}
			if !providerargs.HasValue(request.Args, providerargs.ClaudeSessionIDFlag) {
				t.Fatalf("Claude create args do not contain a preallocated session id: %q", request.Args)
			}
			hasFullAccess := slices.Contains(request.Args, "--dangerously-skip-permissions")
			if hasFullAccess != test.fullAccess {
				t.Fatalf("full access = %v in args %q, want %v", hasFullAccess, request.Args, test.fullAccess)
			}
		})
	}
}

func TestInheritedClaudeChildSendsItsPositionalRequestThroughStructuredInput(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const parent = "11111111-2222-4333-8444-555555555555"
	const prompt = "Review the authentication boundary"
	var request createSessionRequest
	var inputs []string
	submitted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, httpRequest *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case httpRequest.Method == http.MethodPost && httpRequest.URL.Path == "/api/sessions":
			if got := httpRequest.Header.Get("X-Sessions-Creator-Session"); got != parent {
				t.Errorf("creator header = %q, want %q", got, parent)
			}
			if err := json.NewDecoder(httpRequest.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"id":"` + id + `"}`))
		case httpRequest.Method == http.MethodGet && httpRequest.URL.Path == "/api/sessions":
			lastUser := any(nil)
			if submitted {
				lastUser = int64(2)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "claude", "tool": "claude-code", "lastUserMessageAt": lastUser,
			}}})
		case httpRequest.Method == http.MethodGet && httpRequest.URL.Path == "/api/sessions/"+id+"/events":
			events := []any{}
			if submitted {
				events = append(events, map[string]any{
					"type": "user", "message": map[string]any{"role": "user", "content": prompt},
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		case httpRequest.Method == http.MethodPost && httpRequest.URL.Path == "/api/sessions/"+id+"/submit":
			var body map[string]string
			if err := json.NewDecoder(httpRequest.Body).Decode(&body); err != nil {
				t.Errorf("decode input request: %v", err)
			}
			inputs = append(inputs, body["data"], "\r")
			submitted = true
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(response, httpRequest)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", parent)
	t.Setenv("SESSIONS_OWNER_ID", "")

	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "new", "--tool", "claude", prompt},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if request.Kind != "claude-structured" {
		t.Fatalf("create kind = %q, want %q", request.Kind, "claude-structured")
	}
	if slices.Contains(request.Args, prompt) {
		t.Fatalf("positional prompt leaked into structured Claude args: %q", request.Args)
	}
	if want := []string{prompt, "\r"}; !reflect.DeepEqual(inputs, want) {
		t.Fatalf("input sequence = %q, want %q", inputs, want)
	}
}

func TestCodexNewSurfacesCatalogValidationErrorClearly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/sessions" || request.Method != http.MethodPost {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"error":"model \"missing\" not available; valid: [alpha, beta]"}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "new", "--tool", "codex", "--model", "missing"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("invalid model exited 0: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got := stderr.String(); got != "sessions: model \"missing\" not available; valid: [alpha, beta]\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestLastAndTranscriptJSONShapes(t *testing.T) {
	const id = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	events := []any{
		map[string]any{
			"type": "user", "timestamp": "2026-07-16T20:00:00.000Z",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
		},
		map[string]any{
			"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ignored"}}},
		},
		map[string]any{
			"type": "assistant", "timestamp": "2026-07-16T20:00:01.000Z",
			"message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "answer"},
				map[string]any{"type": "tool_use", "name": "Read"},
			}},
		},
		map[string]any{
			"type": "assistant", "timestamp": "2026-07-16T20:00:01.100Z",
			"message": map[string]any{"role": "assistant", "content": []any{}, "usage": map[string]any{"output_tokens": 1}},
		},
		map[string]any{
			"type": "assistant", "timestamp": "2026-07-16T20:00:01.200Z",
			"message": map[string]any{"role": "assistant", "content": []any{}, "stop_reason": "end_turn"},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "claude", "args": []any{}, "cwd": "/tmp", "createdAt": 1,
				"tool": "claude-code", "working": false,
			}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		command string
		want    string
	}{
		{
			"last",
			`[{"role":"user","text":"hello","timestamp":"2026-07-16T20:00:00.000Z"},{"role":"assistant","text":"answer","timestamp":"2026-07-16T20:00:01.000Z"}]`,
		},
		{
			"transcript",
			`[{"role":"user","text":"hello","timestamp":"2026-07-16T20:00:00.000Z"},{"role":"assistant","text":"answer","timestamp":"2026-07-16T20:00:01.000Z","toolCalls":["Read"]}]`,
		},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "--json", test.command, id[:8]}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			assertJSONEqual(t, stdout.String(), test.want)
		})
	}
}

func assertJSONEqual(t *testing.T, actual, expected string) {
	t.Helper()
	var actualValue, expectedValue any
	if err := json.Unmarshal([]byte(actual), &actualValue); err != nil {
		t.Fatalf("decode actual: %v\n%s", err, actual)
	}
	if err := json.Unmarshal([]byte(expected), &expectedValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualValue, expectedValue) {
		t.Fatalf("JSON mismatch\nactual:   %s\nexpected: %s", actual, expected)
	}
}

func TestInputIsSendIncludingJSONAndUnknownOptionRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	id := "25000000-0000-4000-8000-000000000001"
	submitted := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + id + `","cmd":"/bin/sh"}]}`))
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/"+id+"/submit":
			var body struct {
				Data string `json:"data"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode submit body: %v", err)
			}
			submitted = append(submitted, body.Data)
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	// --json is a flag for input exactly as it is for send, never message text.
	for _, arguments := range [][]string{
		{"--host", server.URL, "input", id, "--json", "hi"},
		{"--host", server.URL, "--json", "input", id, "hi"},
		{"--host", server.URL, "--json", "send", id, "hi"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("%v exit=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("%v did not print JSON: %v\n%s", arguments, err, stdout.String())
		}
		if decoded["confidence"] != "unconfirmed" {
			t.Fatalf("%v JSON = %v", arguments, decoded)
		}
	}
	if !slices.Equal(submitted, []string{"hi", "hi", "hi"}) {
		t.Fatalf("submitted text = %#v", submitted)
	}

	// An unknown leading option is refused instead of typed into the session.
	submitted = submitted[:0]
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "input", id, "--nope", "hi"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "unknown send option --nope") {
		t.Fatalf("unknown option exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(submitted) != 0 {
		t.Fatalf("refused send still submitted %#v", submitted)
	}

	// -- sends dashed text verbatim, and a flag inside the message stays text.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "input", id, "--", "--json", "please"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("separator exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "send", id, "run make", "--fast"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("trailing flag-like word exit=%d stderr=%q", code, stderr.String())
	}
	if !slices.Equal(submitted, []string{"--json please", "run make --fast"}) {
		t.Fatalf("submitted text = %#v", submitted)
	}
}
