package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const adoptedLaneID = "44444444-4444-4444-4444-444444444445"
const adoptedSecondLaneID = "55555555-5555-5555-5555-555555555556"
const adoptedSessionID = "66666666-6666-6666-6666-666666666667"

// A lane can complete while startup attaches it. Before adoption, the lane
// listing and manifest legitimately contain nothing. After adoption the same
// session lookup must retain its kind, even if the live-only list omits it.
func adoptionWaitServer(t *testing.T, exitCode, manifestStatus int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var listings, manifests atomic.Int64
	lane := func(id string) map[string]any {
		return map[string]any{"id": id, "kind": "lane", "tool": "lane", "cmd": "sh", "exited": true}
	}
	ordinary := map[string]any{
		"id": adoptedSessionID, "tool": "codex", "cmd": "codex", "working": false, "lastSummary": "ordinary result",
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/lanes":
			lanes := []any{}
			if listings.Load() >= 2 {
				lanes = append(lanes, lane(adoptedLaneID), lane(adoptedSecondLaneID))
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"lanes": lanes})
		case strings.HasPrefix(request.URL.Path, "/api/lanes/") && strings.HasSuffix(request.URL.Path, "/manifest"):
			if request.URL.Path != "/api/lanes/"+adoptedLaneID+"/manifest" && request.URL.Path != "/api/lanes/"+adoptedSecondLaneID+"/manifest" {
				http.NotFound(response, request)
				return
			}
			manifests.Add(1)
			if listings.Load() < 2 {
				http.NotFound(response, request)
				return
			}
			response.WriteHeader(manifestStatus)
			if manifestStatus != http.StatusOK {
				_ = json.NewEncoder(response).Encode(map[string]any{"error": "runner unreachable: inspect recovery before retrying"})
				return
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"exit_code": exitCode, "duration_ms": 20, "last_output_tail": "complete\n",
			})
		case request.URL.Path == "/api/health":
			phase := "loading"
			if listings.Load() >= 2 {
				phase = "ready"
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "startup": map[string]any{"phase": phase}})
		case request.URL.Path == "/api/sessions":
			count := listings.Add(1)
			sessions := []any{}
			if count >= 2 {
				sessions = append(sessions, ordinary)
				if request.URL.Query().Get("include_exited") == "1" {
					sessions = append(sessions, lane(adoptedLaneID), lane(adoptedSecondLaneID))
				}
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": sessions})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, &manifests
}

func isolateAdoptionWait(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(root, "ledger.sqlite3"))
}

func TestWaitRetainsLaneKindAfterStartupAdoption(t *testing.T) {
	tests := []struct {
		name string
		args []string
		join bool
	}{
		{"full-id", []string{adoptedLaneID}, false},
		{"short-prefix", []string{adoptedLaneID[:8]}, false},
		{"lane-fanout", []string{adoptedLaneID, adoptedSecondLaneID, "--all"}, true},
		{"short-prefix-fanout", []string{adoptedLaneID[:8], adoptedSecondLaneID[:8], "--all"}, true},
		{"mixed-join", []string{adoptedLaneID[:8], adoptedSessionID, "--all", "--idle", "0s"}, true},
		{"lane-race", []string{adoptedLaneID[:8], adoptedSecondLaneID, "--any"}, false},
	}
	for _, test := range tests {
		for _, exit := range []int{0, 7} {
			t.Run(test.name+map[int]string{0: "/success", 7: "/failed"}[exit], func(t *testing.T) {
				isolateAdoptionWait(t)
				server, manifests := adoptionWaitServer(t, exit, http.StatusOK)
				var stdout, stderr bytes.Buffer
				args := append([]string{"--host", server.URL, "--json", "wait"}, test.args...)
				args = append(args, "--timeout", "5s")
				code := run(args, strings.NewReader(""), &stdout, &stderr)
				wantCode, wantReason := exit, waitReasonExited
				if exit != 0 {
					wantReason = waitReasonFailed
					if test.join {
						wantCode = exitTargetUnavailable
					}
				}
				if code != wantCode || manifests.Load() < 1 {
					t.Fatalf("exit=%d want=%d manifestReads=%d stdout=%s stderr=%s", code, wantCode, manifests.Load(), stdout.String(), stderr.String())
				}
				outcomes := []waitOutcome{}
				if test.join {
					join := decodeWaitJoin(t, stdout.String())
					outcomes = join.Results
					if join.Reason != wantReason || join.Waited != 2 {
						t.Fatalf("adopted join = %+v", join)
					}
				} else {
					outcomes = append(outcomes, decodeWaitOutcome(t, stdout.String()))
				}
				for _, outcome := range outcomes {
					if outcome.Session == adoptedSessionID {
						if outcome.Kind != waitKindSession || outcome.Reason != waitReasonIdle {
							t.Fatalf("ordinary session changed semantics: %+v", outcome)
						}
						continue
					}
					if outcome.Kind != waitKindLane || outcome.Reason != wantReason || outcome.Lane == nil || outcome.Lane.ExitCode != exit || outcome.Code != exit {
						t.Fatalf("adopted lane lost completion semantics: %+v", outcome)
					}
				}
			})
		}
	}
}

func TestAdoptedLaneRejectsSessionOnlyIdleOption(t *testing.T) {
	isolateAdoptionWait(t)
	server, _ := adoptionWaitServer(t, 0, http.StatusOK)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", adoptedLaneID[:8], "--idle", "0s"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String()+stderr.String(), "--idle describes a settling session, not a lane") {
		t.Fatalf("adopted lane accepted session-only option: exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestAdoptedLaneManifestFailureIsNotSuccessfulIdle(t *testing.T) {
	isolateAdoptionWait(t)
	server, _ := adoptionWaitServer(t, 0, http.StatusInternalServerError)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", adoptedLaneID, "--timeout", "5s"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 || strings.Contains(stdout.String(), `"reason":"idle"`) || !strings.Contains(stdout.String()+stderr.String(), "unavailable") {
		t.Fatalf("manifest failure was hidden: exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
