package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pausedLaneID is a reboot-paused record whose runner cannot be recreated.
const pausedLaneID = "11111111-2222-4333-8444-555555555555"

// pausedRefusal is the body the daemon sends for a read of such a lane; the
// same shape internal/api produces in paused_read_test.go and the one
// runtime/CONTRACT/http-api.md documents.
func pausedRefusal(id string) map[string]string {
	return map[string]string{
		"code":      "SESSION_NEEDS_RECREATE",
		"sessionId": id,
		"error":     "session is paused after reboot and could not be restarted: this machine cannot restart a paused session in place",
		"action":    "sessions resume " + id,
	}
}

// pausedLaneDaemon answers the way a daemon holding one reboot-paused record
// does: the lane is listed, because a paused record stays visible and findable,
// and only a read of its runtime refuses. Answering 409 to everything would
// have let a failure during id resolution pass for a failed read.
func pausedLaneDaemon(t *testing.T, reads *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("content-type", "application/json")
		path := request.URL.Path
		switch {
		case path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": pausedLaneID, "name": "Reboot-paused work", "cmd": "claude", "args": []string{},
				"cwd": "/Users/example/project", "tool": "claude-code",
				"unreachable": true, "unreachable_reason": "restart-restore-pending",
				"idle_reason": "needs-recovery",
			}}})
		case path == "/api/lanes":
			// This daemon has no headless lane by that id, so `last` falls
			// through to the conversation read rather than a lane manifest.
			response.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(response).Encode(map[string]any{"error": "not found"})
		case strings.HasPrefix(path, "/api/sessions/"+pausedLaneID+"/"):
			*reads = append(*reads, path)
			response.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(response).Encode(pausedRefusal(pausedLaneID))
		default:
			response.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(response).Encode(map[string]any{"error": "not found", "path": path})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A read of a paused lane must not look like a quiet, successful, empty one.
// This is the reboot report itself: snap, tail and last printed nothing and
// exited 0, so an unrecoverable lane read as an idle one.
func TestPausedLaneReadsFailLoudlyInsteadOfPrintingNothing(t *testing.T) {
	for _, command := range [][]string{
		{"snap", pausedLaneID},
		{"tail", pausedLaneID},
		{"last", pausedLaneID},
	} {
		t.Run(command[0], func(t *testing.T) {
			var requested []string
			server := pausedLaneDaemon(t, &requested)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SESSIONS_HOST", server.URL)

			var stdout, stderr bytes.Buffer
			code := run(command, strings.NewReader(""), &stdout, &stderr)

			if code == 0 {
				t.Fatalf("%v exited 0; stdout=%q stderr=%q", command, stdout.String(), stderr.String())
			}
			if code != exitTargetUnavailable {
				t.Fatalf("%v exit = %d, want %d", command, code, exitTargetUnavailable)
			}
			// Nothing that could be mistaken for the session's own output.
			if strings.TrimSpace(stdout.String()) != "" {
				t.Fatalf("%v printed %q as if it had read the session", command, stdout.String())
			}
			for _, fragment := range []string{
				"SESSION_NEEDS_RECREATE",
				"could not be restarted",
				"sessions resume " + pausedLaneID,
			} {
				if !strings.Contains(stderr.String(), fragment) {
					t.Fatalf("%v stderr %q is missing %q", command, stderr.String(), fragment)
				}
			}
			// The refusal came from reading this lane's runtime, not from
			// failing to find it: the lane resolved first.
			if len(requested) == 0 {
				t.Fatalf("%v never read the paused lane's runtime; stderr=%q", command, stderr.String())
			}
		})
	}
}

// The reference the refusal prints is the one that resumes that lane, and it
// arrives at the resume boundary unchanged. Nothing else is started.
func TestTheSuggestedResumeReferenceNamesTheSameLane(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var reads []string
	readServer := pausedLaneDaemon(t, &reads)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SESSIONS_HOST", readServer.URL)
	if code := run([]string{"snap", pausedLaneID}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("paused snapshot exited 0: %q", stdout.String())
	}

	// Take the command the refusal printed, exactly as a person would read it.
	suggested := suggestedCommand(t, stderr.String())
	if len(suggested) != 3 || suggested[0] != "sessions" || suggested[1] != "resume" {
		t.Fatalf("suggested command = %v", suggested)
	}

	var adopted map[string]any
	var adoptPaths []string
	resumeServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		adoptPaths = append(adoptPaths, request.Method+" "+request.URL.Path)
		if request.URL.Path == "/api/recovery/adopt" {
			_ = json.NewDecoder(request.Body).Decode(&adopted)
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "laneId": "successor-lane"})
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{})
	}))
	defer resumeServer.Close()
	t.Setenv("SESSIONS_HOST", resumeServer.URL)

	var resumeOut, resumeErr bytes.Buffer
	if code := run(suggested[1:], strings.NewReader(""), &resumeOut, &resumeErr); code != 0 {
		t.Fatalf("%v exit=%d stdout=%q stderr=%q", suggested, code, resumeOut.String(), resumeErr.String())
	}
	if adopted["historyId"] != pausedLaneID {
		t.Fatalf("resume asked for %#v, want the exact paused lane %q", adopted["historyId"], pausedLaneID)
	}
	// One resume of one lane: a failed read never becomes a second start.
	starts := 0
	for _, path := range adoptPaths {
		if strings.HasPrefix(path, "POST /api/sessions") || path == "POST /api/recovery/adopt" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("resume produced %d starts: %v", starts, adoptPaths)
	}
}

// suggestedCommand reads back the "next: sessions resume <id>" the CLI printed.
func suggestedCommand(t *testing.T, stderr string) []string {
	t.Helper()
	const marker = "next: "
	index := strings.Index(stderr, marker)
	if index < 0 {
		t.Fatalf("no next step in %q", stderr)
	}
	line := strings.TrimSpace(strings.SplitN(stderr[index+len(marker):], "\n", 2)[0])
	return strings.Fields(line)
}
