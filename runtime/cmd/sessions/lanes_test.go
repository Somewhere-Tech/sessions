package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunPostsHeadlessLaneRequest(t *testing.T) {
	const creator = "00000000-0000-4000-8000-000000000077"
	t.Setenv("SESSIONS_SESSION_ID", creator)
	t.Setenv("SESSIONS_OWNER_ID", "")
	root := t.TempDir()
	spec := filepath.Join(root, "spec.md")
	if err := os.WriteFile(spec, []byte("lane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var posted runLaneRequest
	var creatorHeader string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/lanes" || request.Method != http.MethodPost {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Error(err)
		}
		creatorHeader = request.Header.Get("X-Sessions-Creator-Session")
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"00000000-0000-4000-8000-000000000001"}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "run", "--name", "test lane", "--cwd", root, "--spec", spec,
		"--", "/bin/sh", "-c", "printf marker",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("run exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if posted.Kind != "lane" || posted.Name != "test lane" || posted.Cwd != root || posted.SpecPath != spec ||
		posted.Cmd != "/bin/sh" || len(posted.Args) != 2 || posted.Args[1] != "printf marker" {
		t.Fatalf("posted lane = %#v", posted)
	}
	if creatorHeader != creator {
		t.Fatalf("creator header = %q, want %q", creatorHeader, creator)
	}
}

func TestRunExplicitOwnerRequiresDetachAndForwardsExternalRoot(t *testing.T) {
	const inherited = "00000000-0000-4000-8000-000000000088"
	t.Setenv("SESSIONS_SESSION_ID", inherited)
	t.Setenv("SESSIONS_OWNER_ID", "")
	root := t.TempDir()
	var ownerHeader, sessionHeader string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		ownerHeader = request.Header.Get("X-Sessions-Owner-ID")
		sessionHeader = request.Header.Get("X-Sessions-Creator-Session")
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"00000000-0000-4000-8000-000000000009"}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "run", "--cwd", root, "--owner", "team:mine", "--", "/bin/sh"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || requests != 0 || !strings.Contains(stderr.String(), "--detach") {
		t.Fatalf("undetached owner exit=%d requests=%d stderr=%q", code, requests, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "run", "--cwd", root, "--owner", "team:mine", "--detach", "--", "/bin/sh"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || requests != 1 {
		t.Fatalf("detached owner exit=%d requests=%d stdout=%q stderr=%q", code, requests, stdout.String(), stderr.String())
	}
	if ownerHeader != "team:mine" || sessionHeader != "" {
		t.Fatalf("owner header=%q session header=%q", ownerHeader, sessionHeader)
	}
}

func TestLaneMineResolutionPrecedenceSubtreeDirectAndOwnerIsolation(t *testing.T) {
	const (
		rootSession = "00000000-0000-4000-8000-000000000001"
		child       = "00000000-0000-4000-8000-000000000002"
		grandchild  = "00000000-0000-4000-8000-000000000003"
		externalA   = "00000000-0000-4000-8000-000000000004"
		externalB   = "00000000-0000-4000-8000-000000000005"
		userLane    = "00000000-0000-4000-8000-000000000006"
	)
	userID := "uid:424242" // Deliberately differs from the CLI uid: daemon hint wins.
	lanes := []laneView{
		{session: session{ID: child, CreatorKind: "session", CreatorID: rootSession, CreatorAncestry: []string{rootSession}, RootCreatorKind: "user", RootCreatorID: userID}},
		{session: session{ID: grandchild, CreatorKind: "session", CreatorID: child, CreatorAncestry: []string{child, rootSession}, RootCreatorKind: "user", RootCreatorID: userID}},
		{session: session{ID: externalA, CreatorKind: "external", CreatorID: "alpha", RootCreatorKind: "external", RootCreatorID: "alpha"}},
		{session: session{ID: externalB, CreatorKind: "external", CreatorID: "beta", RootCreatorKind: "external", RootCreatorID: "beta"}},
		{session: session{ID: userLane, CreatorKind: "user", CreatorID: userID, RootCreatorKind: "user", RootCreatorID: userID}},
	}

	t.Run("mine session is transitive by default", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", rootSession)
		t.Setenv("SESSIONS_OWNER_ID", "")
		assertFilteredLaneIDs(t, lanes, []string{"--mine"}, child, grandchild)
		assertFilteredLaneIDs(t, lanes, []string{"--mine", "--direct"}, child)
	})
	t.Run("subtree resolves descendants", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", "")
		t.Setenv("SESSIONS_OWNER_ID", "")
		assertFilteredLaneIDs(t, lanes, []string{"--subtree", child}, grandchild)
	})
	t.Run("owner environment wins over session", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", rootSession)
		t.Setenv("SESSIONS_OWNER_ID", "alpha")
		assertFilteredLaneIDs(t, lanes, []string{"--mine"}, externalA)
	})
	t.Run("explicit owner needs detach from inherited session", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", rootSession)
		t.Setenv("SESSIONS_OWNER_ID", "")
		if _, err := parseLaneListOptions([]string{"--mine", "--owner", "beta"}); err == nil || !strings.Contains(err.Error(), "--detach") {
			t.Fatalf("owner conflict err=%v", err)
		}
		assertFilteredLaneIDs(t, lanes, []string{"--mine", "--owner", "beta", "--detach"}, externalB)
	})
	t.Run("plain terminal falls back to daemon uid principal", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", "")
		t.Setenv("SESSIONS_OWNER_ID", "")
		assertFilteredLaneIDs(t, lanes, []string{"--mine"}, child, grandchild, userLane)
	})
	t.Run("all remains global", func(t *testing.T) {
		t.Setenv("SESSIONS_SESSION_ID", rootSession)
		t.Setenv("SESSIONS_OWNER_ID", "alpha")
		assertFilteredLaneIDs(t, lanes, []string{"--all"}, child, grandchild, externalA, externalB, userLane)
	})
}

func assertFilteredLaneIDs(t *testing.T, lanes []laneView, args []string, want ...string) {
	t.Helper()
	options, err := parseLaneListOptions(args)
	if err != nil {
		t.Fatal(err)
	}
	daemonUserID := "uid:" + strconv.Itoa(os.Getuid())
	for _, lane := range lanes {
		if lane.RootCreatorKind == "user" {
			daemonUserID = lane.RootCreatorID
			break
		}
	}
	got, err := filterLaneViews(lanes, options, daemonUserID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, lane := range got {
		ids = append(ids, lane.ID)
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("filtered ids = %v, want %v", ids, want)
	}
}

func TestLaneWaitAnyUsesSharedCompositionAndWinningExitCode(t *testing.T) {
	const first = "00000000-0000-4000-8000-000000000001"
	const second = "00000000-0000-4000-8000-000000000002"
	started := time.Now()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[{"id":"` + first + `","kind":"lane","tool":"lane:sh"},{"id":"` + second + `","kind":"lane","tool":"lane:sh"}]}`))
		case "/api/lanes/" + first + "/manifest":
			mu.Lock()
			ready := time.Since(started) >= 500*time.Millisecond
			mu.Unlock()
			if !ready {
				response.WriteHeader(http.StatusConflict)
				_, _ = response.Write([]byte(`{"error":"running"}`))
				return
			}
			_, _ = response.Write([]byte(`{"exit_code":4,"duration_ms":500,"last_output_tail":"first\n","spec_path":""}`))
		case "/api/lanes/" + second + "/manifest":
			mu.Lock()
			ready := time.Since(started) >= 100*time.Millisecond
			mu.Unlock()
			if !ready {
				response.WriteHeader(http.StatusConflict)
				_, _ = response.Write([]byte(`{"error":"running"}`))
				return
			}
			_, _ = response.Write([]byte(`{"exit_code":6,"duration_ms":100,"last_output_tail":"second\n","spec_path":""}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", server.URL, "wait", first, second, "--any", "--summary", "--timeout", "2s", "--json",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 6 || stderr.Len() != 0 {
		t.Fatalf("wait --any exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output struct {
		ID       string `json:"id"`
		ExitCode int    `json:"exit_code"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.ID != second || output.ExitCode != 6 || output.Summary != "second" {
		t.Fatalf("wait --any output = %#v", output)
	}
}
