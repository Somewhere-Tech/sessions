package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	delegateA = "31000000-0000-4000-8000-000000000001"
	delegateB = "31000000-0000-4000-8000-000000000002"
	laneA     = "31000000-0000-4000-8000-0000000000a1"
)

func idleSessionJSON(id string) string {
	return `{"id":"` + id + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"codex","working":false,"lastSummary":"done"}`
}

// delegationServer answers the daemon endpoints a fan-out wait touches. The
// session list is produced per call so a test can make a delegate disappear
// mid-wait, which is what a delegate that dies actually does.
func delegationServer(t *testing.T, sessions func(call int) string, lanes string, manifests map[string]func(call int) (int, string)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	sessionCalls, manifestCalls := 0, map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case path == "/api/sessions":
			mu.Lock()
			sessionCalls++
			call := sessionCalls
			mu.Unlock()
			body := `{"sessions":[]}`
			if sessions != nil {
				body = sessions(call)
			}
			_, _ = response.Write([]byte(body))
		case path == "/api/lanes":
			body := lanes
			if body == "" {
				body = `{"lanes":[]}`
			}
			_, _ = response.Write([]byte(body))
		case strings.HasPrefix(path, "/api/lanes/") && strings.HasSuffix(path, "/manifest"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/lanes/"), "/manifest")
			produce, known := manifests[id]
			if !known {
				http.NotFound(response, request)
				return
			}
			mu.Lock()
			manifestCalls[id]++
			call := manifestCalls[id]
			mu.Unlock()
			code, body := produce(call)
			response.WriteHeader(code)
			_, _ = response.Write([]byte(body))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func decodeWaitJoin(t *testing.T, raw string) waitJoinOutcome {
	t.Helper()
	var join waitJoinOutcome
	if err := json.Unmarshal([]byte(raw), &join); err != nil {
		t.Fatalf("--all did not emit a decodable join: %v (raw %q)", err, raw)
	}
	return join
}

// A delegator that fanned work out to several delegates could not join them:
// multi-target wait refused anything but --any, which returns only the first
// finisher, so the rest had to be re-waited one at a time and a delegate that
// died in the meantime looked exactly like one still working.
func TestWaitAllReturnsOneResultPerDelegateInOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	// Both delegates exist while they are resolved; the second then vanishes.
	both := `{"sessions":[` + idleSessionJSON(delegateA) + `,` +
		`{"id":"` + delegateB + `","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":1,"tool":"codex","working":true}]}`
	server := delegationServer(t, func(call int) string {
		if call <= 2 {
			return both
		}
		return `{"sessions":[` + idleSessionJSON(delegateA) + `]}`
	}, "", nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "wait", delegateA, delegateB,
		"--all", "--idle", "0s", "--timeout", "1h",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitTargetUnavailable {
		t.Fatalf("join exit=%d, want %d — one delegate is gone, so the join cannot be a success",
			code, exitTargetUnavailable)
	}
	join := decodeWaitJoin(t, stdout.String())
	if join.OK {
		t.Fatal("join reported ok:true while one delegate was gone")
	}
	if join.Kind != waitKindJoin || join.Waited != 2 || len(join.Results) != 2 {
		t.Fatalf("join = %+v, want kind %q over 2 targets", join, waitKindJoin)
	}
	if join.Reason != waitReasonGone {
		t.Fatalf("aggregate reason = %q, want the worst outcome %q", join.Reason, waitReasonGone)
	}
	// Order follows the command line so a caller can zip the results against
	// the list it fanned out over.
	if join.Results[0].Session != delegateA || join.Results[1].Session != delegateB {
		t.Fatalf("results named %q, %q — want them in the order requested",
			join.Results[0].Session, join.Results[1].Session)
	}
	if !join.Results[0].OK || join.Results[0].Reason != waitReasonIdle {
		t.Fatalf("first delegate = %+v, want an idle success", join.Results[0])
	}
	if join.Results[1].OK || join.Results[1].Reason != waitReasonGone {
		t.Fatalf("second delegate = %+v, want ok:false reason gone", join.Results[1])
	}
}

// A join of delegates that all finished is a success, and each result is the
// same envelope a single wait returns.
func TestWaitAllSucceedsWhenEveryDelegateFinished(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	body := `{"sessions":[` + idleSessionJSON(delegateA) + `,` + idleSessionJSON(delegateB) + `]}`
	server := delegationServer(t, func(int) string { return body }, "", nil)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "wait", delegateA, delegateB,
		"--all", "--idle", "0s", "--timeout", "1h",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSatisfied {
		t.Fatalf("join exit=%d stderr=%q, want %d", code, stderr.String(), exitSatisfied)
	}
	join := decodeWaitJoin(t, stdout.String())
	if !join.OK || join.Reason != waitReasonIdle || len(join.Results) != 2 {
		t.Fatalf("join = %+v, want ok:true over two idle delegates", join)
	}
	for _, result := range join.Results {
		if result.Kind != waitKindSession || result.Session == "" {
			t.Fatalf("result %+v does not name its target and kind", result)
		}
	}
}

// Delegation is not all one kind: some delegates are interactive sessions and
// some are headless lanes, and a join has to cover both in one call.
func TestWaitAllJoinsLanesAndSessionsTogether(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	server := delegationServer(t,
		func(int) string { return `{"sessions":[` + idleSessionJSON(delegateA) + `]}` },
		`{"lanes":[{"id":"`+laneA+`","kind":"lane","tool":"lane:sh"}]}`,
		map[string]func(int) (int, string){
			laneA: func(call int) (int, string) {
				if call < 2 {
					return http.StatusConflict, `{"error":"running"}`
				}
				return http.StatusOK, `{"exit_code":0,"duration_ms":120,"last_output_tail":"built\n","spec_path":""}`
			},
		})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "wait", laneA, delegateA,
		"--all", "--idle", "0s", "--timeout", "10s",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSatisfied {
		t.Fatalf("mixed join exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	join := decodeWaitJoin(t, stdout.String())
	if !join.OK || len(join.Results) != 2 {
		t.Fatalf("mixed join = %+v", join)
	}
	if join.Results[0].Kind != waitKindLane || join.Results[0].Reason != waitReasonExited {
		t.Fatalf("lane result = %+v, want kind lane reason exited", join.Results[0])
	}
	if join.Results[0].Lane == nil || join.Results[0].Lane.LastOutputTail != "built\n" {
		t.Fatalf("lane payload = %+v, want the manifest nested under lane", join.Results[0].Lane)
	}
	if join.Results[1].Kind != waitKindSession {
		t.Fatalf("session result = %+v, want kind session", join.Results[1])
	}
}

// One parser has to read both. The lane answer used to share no field at all
// with the session answer — no ok, no reason, no session — and spelled its
// timings in snake_case while the session answer used camelCase.
func TestLaneAndSessionWaitAnswerWithTheSameKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	server := delegationServer(t,
		func(int) string { return `{"sessions":[` + idleSessionJSON(delegateA) + `]}` },
		`{"lanes":[{"id":"`+laneA+`","kind":"lane","tool":"lane:sh"}]}`,
		map[string]func(int) (int, string){
			laneA: func(int) (int, string) {
				return http.StatusOK, `{"exit_code":0,"duration_ms":5,"last_output_tail":"ok\n","spec_path":""}`
			},
		})

	documentFor := func(args ...string) map[string]any {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--host", server.URL, "--json", "wait"}, args...),
			strings.NewReader(""), &stdout, &stderr); code != exitSatisfied {
			t.Fatalf("wait %v exit=%d stderr=%q", args, code, stderr.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("wait %v did not emit JSON: %v (%q)", args, err, stdout.String())
		}
		return decoded
	}

	lane := documentFor(laneA, "--timeout", "5s")
	session := documentFor(delegateA, "--idle", "0s", "--timeout", "5s")
	for _, key := range []string{"ok", "kind", "reason", "session", "working"} {
		if _, present := lane[key]; !present {
			t.Fatalf("lane answer is missing the shared key %q: %v", key, lane)
		}
		if _, present := session[key]; !present {
			t.Fatalf("session answer is missing the shared key %q: %v", key, session)
		}
	}
	if lane["kind"] != waitKindLane || session["kind"] != waitKindSession {
		t.Fatalf("kind does not discriminate the two: lane=%v session=%v", lane["kind"], session["kind"])
	}
	if _, flattened := lane["exit_code"]; flattened {
		t.Fatalf("lane-only payload is still at the top level: %v", lane)
	}
}

// A lane wait keeps propagating the child's own exit code. That is a
// deliberate open product decision, and unifying the envelope must not quietly
// settle it.
func TestLaneWaitStillPropagatesTheChildExitCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	server := delegationServer(t, nil,
		`{"lanes":[{"id":"`+laneA+`","kind":"lane","tool":"lane:sh"}]}`,
		map[string]func(int) (int, string){
			laneA: func(int) (int, string) {
				return http.StatusOK, `{"exit_code":7,"duration_ms":5,"last_output_tail":"boom\n","spec_path":""}`
			},
		})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "wait", laneA, "--timeout", "5s"},
		strings.NewReader(""), &stdout, &stderr); code != 7 {
		t.Fatalf("lane wait exit=%d, want the child's own 7", code)
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.Lane == nil || outcome.Lane.ExitCode != 7 || outcome.Reason != waitReasonFailed {
		t.Fatalf("outcome = %+v, want the exit status nested and reported as failed", outcome)
	}
}

// --idle asks for a settling window, which a lane does not have: its wait ends
// when the process exits. The value used to be parsed and thrown away.
func TestLaneWaitRefusesIdleInsteadOfIgnoringIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := delegationServer(t, nil,
		`{"lanes":[{"id":"`+laneA+`","kind":"lane","tool":"lane:sh"}]}`, nil)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "wait", laneA, "--idle", "5s", "--timeout", "1s"},
		strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "--idle") {
		t.Fatalf("lane --idle exit=%d stderr=%q, want a usage refusal", code, stderr.String())
	}
}

// A --until condition answers in the shared envelope too, and a relative path
// means what it means in the caller's shell. It used to be resolved against the
// delegate's cwd — a directory the caller usually does not know, since
// `sessions new` defaults it to $HOME while `sessions run` inherits the
// caller's — so the wait watched a file that would never exist and timed out
// with nothing to explain it.
func TestWaitUntilFileContainsResolvesAgainstTheCallersDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	callerDir := t.TempDir()
	delegateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(callerDir, "result.txt"), []byte("READY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"sessions":[{"id":"` + delegateA + `","cmd":"codex","cwd":"` + delegateDir +
		`","createdAt":1,"pid":1,"tool":"codex","working":true}]}`
	server := delegationServer(t, func(int) string { return body }, "", nil)
	t.Chdir(callerDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "wait", delegateA,
		"--until-file-contains", "result.txt", "READY", "--timeout", "5s",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitSatisfied {
		t.Fatalf("file condition exit=%d stdout=%q stderr=%q — a relative path must mean the caller's cwd",
			code, stdout.String(), stderr.String())
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if !outcome.OK || outcome.Kind != waitKindFile || outcome.Reason != waitReasonSatisfied {
		t.Fatalf("outcome = %+v, want ok:true kind file-contains reason satisfied", outcome)
	}
	if outcome.Session != delegateA {
		t.Fatalf("session = %q, want the target %q", outcome.Session, delegateA)
	}
	if outcome.Condition == nil || outcome.Condition.File != filepath.Join(callerDir, "result.txt") {
		t.Fatalf("condition = %+v, want the caller-rooted path", outcome.Condition)
	}
}

// A condition that was never observed still names the target it was watching.
// The timeout used to answer with ok, reason, elapsed_ms, and a bare count of
// conditions, so a caller learned that something timed out but never which.
func TestWaitUntilTimeoutNamesItsTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	delegateDir := t.TempDir()
	body := `{"sessions":[{"id":"` + delegateA + `","cmd":"codex","cwd":"` + delegateDir +
		`","createdAt":1,"pid":1,"tool":"codex","working":true}]}`
	server := delegationServer(t, func(int) string { return body }, "", nil)
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "wait", delegateA,
		"--until-file-contains", "never-written.txt", "READY", "--timeout", "300ms",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitWaitTimeout {
		t.Fatalf("condition timeout exit=%d, want %d", code, exitWaitTimeout)
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.OK || outcome.Reason != waitReasonTimeout {
		t.Fatalf("outcome = %+v, want ok:false reason timeout", outcome)
	}
	if outcome.Session != delegateA || outcome.Kind != waitKindFile {
		t.Fatalf("outcome = %+v, want it to name target %q and kind %q", outcome, delegateA, waitKindFile)
	}
}

// ask and send share one delivery path, so they answer the same situation with
// the same document and the same status. Under --json, ask used to return the
// document with status 0 for the very failure it exits 1 on in prose mode.
func TestAskAndSendAgreeOnTheUnconfirmableAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body := `{"sessions":[{"id":"` + delegateA + `","cmd":"bash","cwd":"/tmp","createdAt":1,"pid":1,` +
		`"tool":"shell","working":false}]}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(body))
		case strings.HasSuffix(request.URL.Path, "/submit"):
			_, _ = response.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	keysOf := func(args ...string) (map[string]any, int) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--host", server.URL, "--json"}, args...),
			strings.NewReader(""), &stdout, &stderr)
		var decoded map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("%v did not emit JSON: %v (%q / %q)", args, err, stdout.String(), stderr.String())
		}
		return decoded, code
	}

	askDocument, askCode := keysOf("ask", delegateA, "hello")
	sendDocument, _ := keysOf("send", delegateA, "hello")
	if askCode != exitUsage {
		t.Fatalf("--json ask exit=%d, want %d — the same failure exits 1 without --json", askCode, exitUsage)
	}
	var plainStdout, plainStderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "ask", delegateA, "hello"},
		strings.NewReader(""), &plainStdout, &plainStderr); code != askCode {
		t.Fatalf("ask exits %d in prose mode but %d under --json", code, askCode)
	}
	for key := range sendDocument {
		if _, present := askDocument[key]; !present {
			t.Fatalf("ask dropped send's %q from the shared answer: %v", key, askDocument)
		}
	}
	if askDocument["confidence"] != "unconfirmed" {
		t.Fatalf("ask answer = %v, want send's confidence field", askDocument)
	}
}

// A delegated task that takes longer than 30 seconds is the normal case, and
// the one-shot form used to have no way to say so.
func TestRunWaitAcceptsAnExplicitTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_SESSION_ID", "")
	t.Setenv("SESSIONS_OWNER_ID", "")
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/lanes":
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"id":"` + laneA + `"}`))
		case strings.HasSuffix(request.URL.Path, "/manifest"):
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":"running"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	// A lane that never finishes proves the flag is honored: the wait ends at
	// the timeout that was asked for, not at the 30-second cap.
	started := time.Now()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "--json", "run", "--cwd", root, "--wait", "--timeout", "400ms",
		"--", "/bin/sh", "-c", "sleep 60",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitWaitTimeout {
		t.Fatalf("run --wait --timeout exit=%d stdout=%q, want %d", code, stdout.String(), exitWaitTimeout)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("run --wait ignored --timeout and waited %s", elapsed)
	}
	outcome := decodeWaitOutcome(t, stdout.String())
	if outcome.Reason != waitReasonTimeout || outcome.Session != laneA {
		t.Fatalf("outcome = %+v, want a timeout naming the lane", outcome)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "run", "--cwd", root, "--timeout", "5s", "--", "/bin/sh"},
		strings.NewReader(""), &stdout, &stderr); code != exitUsage {
		t.Fatalf("--timeout without --wait exit=%d, want a usage refusal", code)
	}
}

// --reason takes the next word whatever it is, so a following flag became the
// recorded reason and the flag itself was silently dropped.
func TestKillRefusesAFlagAsItsReason(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		response.Header().Set("Content-Type", "application/json")
		http.NotFound(response, request)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "kill", delegateA, "--reason", "--force"},
		strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("kill --reason --force exit=%d, want a usage refusal", code)
	}
	if requests != 0 {
		t.Fatalf("kill contacted the daemon %d times before refusing the mistake", requests)
	}
	if !strings.Contains(stderr.String(), "--reason") {
		t.Fatalf("stderr = %q, want it to name the option at fault", stderr.String())
	}
}

// A configured listener that will not verify is the most common degraded state
// this command reports, and a --json caller used to get a bare error instead of
// the state the daemon had just described. `remote status` answers the
// analogous case with the configured state and verified:false.
func TestLanStatusJSONReportsAnUnverifiedListener(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/lan" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write([]byte(`{"enabled":true,"url":"http://192.0.2.7:8787",` +
			`"bonjour":{"advertised":true,"service":"_sessions._tcp"}}`))
	}))
	defer server.Close()
	previous := primaryLANIPv4
	primaryLANIPv4 = func() (net.IP, error) { return net.ParseIP("192.0.2.9"), nil }
	t.Cleanup(func() { primaryLANIPv4 = previous })

	var stdout, stderr bytes.Buffer
	run([]string{"--host", server.URL, "--json", "lan", "status"}, strings.NewReader(""), &stdout, &stderr)
	var decoded struct {
		Enabled           bool    `json:"enabled"`
		Verified          bool    `json:"verified"`
		URL               *string `json:"url"`
		VerificationError string  `json:"verificationError"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("--json lan status emitted no JSON: %v (stdout %q stderr %q)",
			err, stdout.String(), stderr.String())
	}
	if !decoded.Enabled || decoded.Verified {
		t.Fatalf("status = %+v, want enabled:true verified:false", decoded)
	}
	if decoded.URL == nil || *decoded.URL != "http://192.0.2.7:8787" {
		t.Fatalf("status dropped the configured URL: %+v", decoded)
	}
	if decoded.VerificationError == "" {
		t.Fatalf("status did not explain why it is unverified: %+v", decoded)
	}
}

// A mistyped option used to be skipped in silence, printing the default 50
// lines as if the request had been understood.
func TestTailRefusesArgumentsItCannotHonor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := delegationServer(t, func(int) string { return `{"sessions":[` + idleSessionJSON(delegateA) + `]}` }, "", nil)
	for _, args := range [][]string{
		{"tail", delegateA, "--lines"},
		{"tail", delegateA, "--folow"},
		{"tail", delegateA, "-n"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"--host", server.URL}, args...), strings.NewReader(""), &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("%v exit=%d stdout=%q, want a usage refusal", args, code, stdout.String())
		}
	}
}

// sync-native is dispatched, so the line a caller reads after a typo has to
// list it.
func TestMachinesUsageListsEverySubcommandItDispatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"machines", "sync-nativ"}, strings.NewReader(""), &stdout, &stderr); code != exitUsage {
		t.Fatalf("unknown subcommand exit=%d", code)
	}
	if !strings.Contains(stderr.String(), "sync-native") {
		t.Fatalf("usage = %q, want it to list sync-native", stderr.String())
	}
	var help bytes.Buffer
	if err := writeCommandHelp(&help, "machines"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "sync-native") {
		t.Fatalf("machines help does not mention sync-native:\n%s", help.String())
	}
}

// An agent inside a Sessions session has no injected skill telling it that
// cross-agent delegation exists, so the CLI's own help has to say so.
func TestHelpMakesCrossAgentDelegationDiscoverable(t *testing.T) {
	var top bytes.Buffer
	if err := writeTopLevelHelp(&top); err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"Delegating", "--from", "--all", "sessions ask"} {
		if !strings.Contains(top.String(), phrase) {
			t.Fatalf("top-level help never mentions %q:\n%s", phrase, top.String())
		}
	}
	var waitHelp bytes.Buffer
	if err := writeCommandHelp(&waitHelp, "wait"); err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"--all", "--any", "kind"} {
		if !strings.Contains(waitHelp.String(), phrase) {
			t.Fatalf("wait help never mentions %q:\n%s", phrase, waitHelp.String())
		}
	}
}
