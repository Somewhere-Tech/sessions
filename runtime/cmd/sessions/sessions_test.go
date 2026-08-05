package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	daemonapi "github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestSessionsMineUnifiesOwnedAgentAndLaneIncludesClosedAndKillNoops(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_OWNER_ID", "team:ownership")
	t.Setenv("SESSIONS_SESSION_ID", "")
	config := cliRecoveryConfig(root)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	agent, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "owned agent", CreatorOwnerID: "team:ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "owned lane", Kind: state.KindLane, CreatorOwnerID: "team:ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "other owner", CreatorOwnerID: "team:other",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(daemonapi.New(config, manager, manager.Push()))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "list", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, agent.ID[:8]) || !strings.Contains(stdout, lane.ID[:8]) ||
		!strings.Contains(stdout, "owned agent") || !strings.Contains(stdout, "owned lane") || strings.Contains(stdout, "other owner") {
		t.Fatalf("sessions --mine exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "owned agent") || strings.Contains(stdout, "owned lane") || strings.Contains(stdout, "other owner") {
		t.Fatalf("ls --mine exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "lanes", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "owned lane") || strings.Contains(stdout, "owned agent") {
		t.Fatalf("lanes --mine exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if err := manager.RequestKill(context.Background(), lane.ID, false); err != nil {
		t.Fatal(err)
	}
	cliWaitFor(t, func() bool {
		current, ok := manager.Get(lane.ID)
		return ok && current.Info().Exited && cliHasLedgerEvent(t, store, lane.ID, ledger.EventRunnerExited)
	})
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "list", "--mine")
	if code != 0 || stderr != "" || strings.Contains(stdout, "owned lane") {
		t.Fatalf("sessions default closed filtering exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "list", "--mine", "--include-closed")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "owned lane") || !strings.Contains(stdout, "exited(0)") {
		t.Fatalf("sessions --include-closed exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "kill", lane.ID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "already exited; nothing to kill") || strings.Contains(stderr, "stale") {
		t.Fatalf("kill exited lane exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

}

func TestSessionsMineLabelsOSUserFallbackAsUserWide(t *testing.T) {
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"20000000-0000-4000-8000-000000000001","cmd":"/bin/sh","cwd":"/tmp","createdAt":1,"tool":"terminal","root_creator_kind":"user","root_creator_id":"uid:424242"}]}`))
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "list", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "ownership scope: OS user uid:424242") ||
		!strings.Contains(stdout, "no SESSIONS_OWNER_ID or SESSIONS_SESSION_ID") {
		t.Fatalf("OS-user fallback exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestSessionTablesAddProfileColumnOnlyWhenNeeded(t *testing.T) {
	profiled := true
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		profileFields := ""
		if profiled {
			profileFields = `,"profile":"work","config_dir":"/profiles/claude/work"`
		}
		_, _ = response.Write([]byte(`{"sessions":[{"id":"22000000-0000-4000-8000-000000000001","name":"agent","description":"","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":1,"tool":"claude-code","idleReason":"completed","lastSummary":"Implemented the requested fleet view."` + profileFields + `}]}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	for _, command := range []string{"list", "ls"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--host", server.URL, command}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
			!strings.Contains(stdout.String(), "PROFILE") || !strings.Contains(stdout.String(), "SUMMARY") ||
			!strings.Contains(stdout.String(), "Implemented the requested fleet view.") || !strings.Contains(stdout.String(), "work") {
			t.Fatalf("%s profile table exit=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
	}
	profiled = false
	for _, command := range []string{"list", "ls"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--host", server.URL, command}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
			strings.Contains(stdout.String(), "PROFILE") {
			t.Fatalf("%s default table exit=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
	}
}

func TestWaitReturnsProviderPromptWithoutTerminalBabysitting(t *testing.T) {
	id := "23000000-0000-4000-8000-000000000001"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write([]byte(`{"sessions":[{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,"tool":"codex","working":false,"idleReason":"needs-input","idleDetail":"Allow the focused regression test to open its local IPC socket?","lastSummary":"Waiting for approval."}]}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "wait", id, "--idle", "1h", "--timeout", "1h", "--summary"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 || stderr.String() != "" || !strings.Contains(stdout.String(), "needs-input — Allow the focused regression test") {
		t.Fatalf("wait prompt exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestKillExitedLaneUsesDurableManifestAsCleanNoop(t *testing.T) {
	id := "21000000-0000-4000-8000-000000000001"
	deleteRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes/"+id+"/manifest":
			_, _ = response.Write([]byte(`{"exit_code":0,"signal":null,"duration_ms":1,"last_output_tail":"done\n","spec_path":""}`))
		case request.Method == http.MethodDelete:
			deleteRequests++
			response.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "kill", id)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "already exited; nothing to kill") || deleteRequests != 0 {
		t.Fatalf("manifest kill noop exit=%d deletes=%d stdout=%q stderr=%q", code, deleteRequests, stdout, stderr)
	}
}

func TestKillForwardsAgentAttributionReasonAndBatchID(t *testing.T) {
	targetA := "23000000-0000-4000-8000-000000000001"
	targetB := "23000000-0000-4000-8000-000000000002"
	initiator := "23000000-0000-4000-8000-000000000099"
	t.Setenv("SESSIONS_SESSION_ID", initiator)
	t.Setenv("SESSIONS_OWNER_ID", "")
	type endBody struct {
		IDs         []string `json:"ids"`
		Reason      string   `json:"reason"`
		OperationID string   `json:"operationId"`
		Force       bool     `json:"force"`
	}
	var captured endBody
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + targetA + `","cmd":"/bin/sh"},{"id":"` + targetB + `","cmd":"/bin/sh"}]}`))
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/end-batch":
			requests++
			if request.Header.Get("X-Sessions-Creator-Session") != initiator {
				t.Errorf("creator header = %q", request.Header.Get("X-Sessions-Creator-Session"))
			}
			if request.Header.Get("X-Sessions-Client") != "sessions-cli" {
				t.Errorf("client header = %q", request.Header.Get("X-Sessions-Client"))
			}
			if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
				t.Errorf("decode end body: %v", err)
			}
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "kill", targetA, targetB, "--reason", "Kill completed lanes")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "killed "+targetA) || !strings.Contains(stdout, "killed "+targetB) {
		t.Fatalf("kill batch exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if requests != 1 || !reflect.DeepEqual(captured.IDs, []string{targetA, targetB}) ||
		captured.Reason != "Kill completed lanes" || captured.OperationID == "" || captured.Force {
		t.Fatalf("captured batch = %#v requests=%d", captured, requests)
	}
	captured = endBody{}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "kill", targetA, targetB)
	if code != 0 || stderr != "" {
		t.Fatalf("reasonless kill batch exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if requests != 2 || captured.Reason != "" || captured.OperationID == "" {
		t.Fatalf("reasonless batch invented attribution: %#v requests=%d", captured, requests)
	}
}

func TestKillSingleForwardsAgentAttributionReasonAndForce(t *testing.T) {
	target := "23000000-0000-4000-8000-000000000003"
	initiator := "23000000-0000-4000-8000-000000000099"
	t.Setenv("SESSIONS_SESSION_ID", initiator)
	t.Setenv("SESSIONS_OWNER_ID", "")
	var captured struct {
		Reason      string `json:"reason"`
		OperationID string `json:"operationId"`
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + target + `","cmd":"/bin/sh"}]}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/api/sessions/"+target:
			requests++
			if request.URL.Query().Get("force") != "1" {
				t.Errorf("force query = %q", request.URL.RawQuery)
			}
			if request.Header.Get("X-Sessions-Creator-Session") != initiator {
				t.Errorf("creator header = %q", request.Header.Get("X-Sessions-Creator-Session"))
			}
			if request.Header.Get("X-Sessions-Client") != "sessions-cli" {
				t.Errorf("client header = %q", request.Header.Get("X-Sessions-Client"))
			}
			if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
				t.Errorf("decode end body: %v", err)
			}
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(
		t, server.URL, "kill", target, "--reason", "Finished delegated work", "--force",
	)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "killed "+target) {
		t.Fatalf("single kill exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if requests != 1 || captured.Reason != "Finished delegated work" || captured.OperationID != "" {
		t.Fatalf("captured single end = %#v requests=%d", captured, requests)
	}
}

func TestLSMineJSONFiltersTypesWithoutRewritingRawFieldCasing(t *testing.T) {
	t.Setenv("SESSIONS_OWNER_ID", "team:json")
	t.Setenv("SESSIONS_SESSION_ID", "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"sessions":[` +
			`{"id":"22000000-0000-4000-8000-000000000001","kind":"","cmd":"/bin/sh","root_creator_kind":"external","root_creator_id":"team:json","futureMixedCase":"kept"},` +
			`{"id":"22000000-0000-4000-8000-000000000002","kind":"lane","cmd":"/bin/sh","root_creator_kind":"external","root_creator_id":"team:json","futureMixedCase":"lane"}` +
			`]}`))
	}))
	defer server.Close()
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "--json", "ls", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"futureMixedCase": "kept"`) || strings.Contains(stdout, `"futureMixedCase": "lane"`) {
		t.Fatalf("ls --mine JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestDescriptionShowsInCleanupListsStatusAndJSON(t *testing.T) {
	t.Setenv("SESSIONS_OWNER_ID", "team:description")
	t.Setenv("SESSIONS_SESSION_ID", "")
	const (
		sessionID   = "23000000-0000-4000-8000-000000000001"
		laneID      = "23000000-0000-4000-8000-000000000002"
		description = "Investigate cleanup behavior with a deliberately long full purpose description"
	)
	sessionJSON := `{"id":"` + sessionID + `","description":"` + description + `","description_source":"explicit","cmd":"/bin/sh","cwd":"/tmp","createdAt":1,"lastDataAt":1,"tool":"terminal","root_creator_kind":"external","root_creator_id":"team:description"}`
	laneJSON := `{"id":"` + laneID + `","kind":"lane","description":"Clean generated release artifacts","description_source":"explicit","cmd":"/bin/sh","cwd":"/tmp","createdAt":1,"lastDataAt":1,"tool":"lane","root_creator_kind":"external","root_creator_id":"team:description"}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[` + sessionJSON + `,` + laneJSON + `]}`))
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[` + laneJSON + `],"user_creator_id":"uid:424242"}`))
		case "/api/sessions/" + sessionID + "/verdict":
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"not found"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "list", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "DESC") ||
		!strings.Contains(stdout, "Investigate cleanup behavior with a del…") ||
		!strings.Contains(stdout, "Clean generated release artifacts") {
		t.Fatalf("sessions cleanup view exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "DESC") || !strings.Contains(stdout, "Investigate cleanup") {
		t.Fatalf("ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "lanes")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "DESC") || !strings.Contains(stdout, "Clean generated release artifacts") {
		t.Fatalf("lanes exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "status", sessionID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "desc     "+description) {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "list", "--mine")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"description": "`+description+`"`) ||
		!strings.Contains(stdout, `"description_source": "explicit"`) {
		t.Fatalf("sessions JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, command := range []string{"ls", "lanes"} {
		stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", command)
		if code != 0 || stderr != "" || !strings.Contains(stdout, `"description"`) ||
			!strings.Contains(stdout, `"description_source": "explicit"`) {
			t.Fatalf("%s JSON exit=%d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "status", sessionID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"description": "`+description+`"`) {
		t.Fatalf("status JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func runOwnershipCLI(t *testing.T, host string, args ...string) (string, string, int) {
	t.Helper()
	arguments := append([]string{"--host", host}, args...)
	var stdout, stderr bytes.Buffer
	code := run(arguments, strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// killStubServer answers the daemon routes cmdKill needs and returns whatever
// the batch responder produces, so tests can model a daemon that refuses,
// partially ends, or silently accepts a batch.
func killStubServer(t *testing.T, ids []string, batch func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	listed := make([]string, 0, len(ids))
	for _, id := range ids {
		listed = append(listed, `{"id":"`+id+`","cmd":"/bin/sh"}`)
	}
	sessionsBody := `{"sessions":[` + strings.Join(listed, ",") + `]}`
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(sessionsBody))
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions/end-batch":
			_, _ = io.Copy(io.Discard, request.Body)
			batch(response)
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/sessions/"):
			_, _ = io.Copy(io.Discard, request.Body)
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
}

func TestKillBatchReportsOnlyTargetsTheDaemonConfirmed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	targetA := "24000000-0000-4000-8000-000000000001"
	targetB := "24000000-0000-4000-8000-000000000002"
	server := killStubServer(t, []string{targetA, targetB}, func(response http.ResponseWriter) {
		_, _ = response.Write([]byte(`{"ok":true,"ids":["` + targetA + `"]}`))
	})
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "kill", targetA, targetB)
	if code != 1 {
		t.Fatalf("partial batch exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "killed "+targetA) || strings.Contains(stdout, "killed "+targetB) {
		t.Fatalf("partial batch claimed an unconfirmed kill: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, targetB) || !strings.Contains(stderr, "did not end") {
		t.Fatalf("partial batch stderr=%q", stderr)
	}

	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "kill", targetA, targetB)
	if code != 1 {
		t.Fatalf("partial batch JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result killResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode kill JSON: %v\n%s", err, stdout)
	}
	if len(result.Items) != 2 || result.OperationID == "" {
		t.Fatalf("kill JSON = %#v", result)
	}
	if result.Items[0].ID != targetA || result.Items[0].Status != killStatusKilled {
		t.Fatalf("first item = %#v", result.Items[0])
	}
	if result.Items[1].ID != targetB || result.Items[1].Status != killStatusFailed || result.Items[1].Reason == "" {
		t.Fatalf("second item = %#v", result.Items[1])
	}
}

func TestKillBatchWithoutConfirmationIsAmbiguousNotSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	targetA := "24000000-0000-4000-8000-000000000003"
	targetB := "24000000-0000-4000-8000-000000000004"
	server := killStubServer(t, []string{targetA, targetB}, func(response http.ResponseWriter) {
		_, _ = response.Write([]byte(`{}`))
	})
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "kill", targetA, targetB)
	if code != 2 {
		t.Fatalf("unconfirmed batch exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "killed ") {
		t.Fatalf("unconfirmed batch claimed a kill: stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "could not confirm") {
		t.Fatalf("unconfirmed batch stderr=%q", stderr)
	}

	server.Close()
	explicitlyRefused := killStubServer(t, []string{targetA, targetB}, func(response http.ResponseWriter) {
		_, _ = response.Write([]byte(`{"ok":false,"error":"guarded batch refused"}`))
	})
	defer explicitlyRefused.Close()
	stdout, stderr, code = runOwnershipCLI(t, explicitlyRefused.URL, "kill", targetA, targetB)
	if code != 1 || strings.Contains(stdout, "killed ") || !strings.Contains(stderr, "guarded batch refused") {
		t.Fatalf("refused batch exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestKillEmitsJSONForOneTargetBeforeOrAfterTheCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	target := "24000000-0000-4000-8000-000000000005"
	server := killStubServer(t, []string{target}, func(response http.ResponseWriter) {
		http.Error(response, `{"error":"batch end requires at least two session ids"}`, http.StatusBadRequest)
	})
	defer server.Close()

	for _, arguments := range [][]string{{"--json", "kill", target}, {"kill", "--json", target}} {
		stdout, stderr, code := runOwnershipCLI(t, server.URL, arguments...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v exit=%d stdout=%q stderr=%q", arguments, code, stdout, stderr)
		}
		var result killResult
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("decode %v JSON: %v\n%s", arguments, err, stdout)
		}
		if len(result.Items) != 1 || result.Items[0].ID != target || result.Items[0].Status != killStatusKilled {
			t.Fatalf("%v result = %#v", arguments, result)
		}
		if result.OperationID != "" {
			t.Fatalf("single kill invented a batch operation id: %#v", result)
		}
	}
}

func TestKillUnknownSessionReportsFailureInBothModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_OWNER_ID", "")
	t.Setenv("SESSIONS_SESSION_ID", "")
	target := "24000000-0000-4000-8000-000000000006"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[],"user_creator_id":"uid:424242"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + target + `","cmd":"/bin/sh"}]}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/api/sessions/"+target:
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"session not found"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "kill", target)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "no session matching") {
		t.Fatalf("unknown kill exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "kill", target)
	if code != 1 {
		t.Fatalf("unknown kill JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result killResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode unknown kill JSON: %v\n%s", err, stdout)
	}
	if len(result.Items) != 1 || result.Items[0].Status != killStatusFailed {
		t.Fatalf("unknown kill result = %#v", result)
	}
}
