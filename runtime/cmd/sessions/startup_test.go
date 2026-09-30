package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// From the Mini, 11 September: three minutes into a restart with 593 sessions,
// `sessions wait <lane>` answered rc 4 "no live session matches" for a lane
// that was running the whole time. The session had not been re-attached yet,
// and the answer did not distinguish that from the session being gone.
func TestWaitDoesNotDeclareALoadingSessionGone(t *testing.T) {
	const id = "44444444-4444-4444-4444-444444444444"
	var listings, served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/health"):
			// Loading until the third listing, then loaded — the daemon
			// finishing its pass while the caller waits.
			phase := "loading"
			if served.Load() > 0 {
				phase = "ready"
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"ok": true, "startup": map[string]any{
					"phase": phase, "loaded": 312, "total": 593, "startedAt": 1_757_000_000_000,
				},
			})
		case request.URL.Path == "/api/sessions":
			count := listings.Add(1)
			sessions := []any{}
			if count >= 4 {
				served.Add(1)
				sessions = append(sessions, map[string]any{
					"id": id, "name": "the lane that existed all along", "cmd": "/bin/sh", "args": []string{},
					"cwd": "/tmp", "createdAt": int64(1), "lastDataAt": int64(1), "tool": "terminal",
					"idleReason": "completed",
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": sessions})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", id, "--timeout", "20s"},
		strings.NewReader(""), &stdout, &stderr)

	if code == 4 {
		t.Fatalf("wait reported the target unavailable while the daemon was loading: %s%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "no live session matches") {
		t.Fatalf("wait told the person a loading session does not exist: %s%s", stdout.String(), stderr.String())
	}
	// It says what it is waiting for, once, on stderr — where it cannot corrupt
	// the JSON document.
	notices := strings.Count(stderr.String(), "still loading sessions")
	if notices != 1 {
		t.Fatalf("the loading notice appeared %d times on stderr: %q", notices, stderr.String())
	}
	if !strings.Contains(stderr.String(), "312 of 593") {
		t.Fatalf("the notice does not say how far along the daemon is: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("wait exit=%d once the session loaded; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

// The same rule for a lookup by id: while the daemon is loading, an absent
// session is one that has not arrived yet.
func TestStatusWaitsForALoadingDaemonRatherThanDenyingTheSession(t *testing.T) {
	const id = "55555555-5555-5555-5555-555555555555"
	var listings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/health"):
			_ = json.NewEncoder(response).Encode(map[string]any{
				"ok": true, "startup": map[string]any{"phase": "loading", "loaded": 10, "total": 593},
			})
		case request.URL.Path == "/api/sessions":
			sessions := []any{}
			if listings.Add(1) >= 3 {
				sessions = append(sessions, map[string]any{
					"id": id, "name": "late arrival", "cmd": "claude", "args": []string{},
					"cwd": "/tmp", "createdAt": int64(1), "lastDataAt": int64(1), "tool": "claude-code",
				})
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": sessions})
		case strings.HasSuffix(request.URL.Path, "/verdict"):
			response.WriteHeader(http.StatusNotFound)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "status", id[:8]}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("status exit=%d while the daemon was loading; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "late arrival") {
		t.Fatalf("status did not answer once the session loaded: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "still loading sessions") {
		t.Fatalf("status waited without saying why: %q", stderr.String())
	}
}

// A daemon that is not loading must still answer the old way: an id that does
// not exist is an id that does not exist, immediately.
func TestAnUnknownSessionIsStillUnknownWhenTheDaemonIsReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/health"):
			_ = json.NewEncoder(response).Encode(map[string]any{
				"ok": true, "startup": map[string]any{"phase": "ready", "loaded": 3, "total": 3},
			})
		case request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "status", "66666666"}, strings.NewReader(""), &stdout, &stderr)

	if code == 0 {
		t.Fatalf("status succeeded for a session that does not exist: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no live session matches") {
		t.Fatalf("stderr = %q, want the unknown-session answer", stderr.String())
	}
	if strings.Contains(stderr.String(), "still loading") {
		t.Fatalf("a ready daemon claimed to be loading: %q", stderr.String())
	}
}

// A daemon too old to report the phase reads as ready. Health must never become
// the reason a command fails.
func TestADaemonWithoutAStartupBlockReadsAsReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	application := &app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, api: client}

	if state := application.daemonStartup(); state.loading() {
		t.Fatalf("a daemon with no startup block read as loading: %#v", state)
	}
}
