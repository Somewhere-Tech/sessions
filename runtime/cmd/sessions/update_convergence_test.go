package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateConvergenceAccountsForAuthoritativeEndings(t *testing.T) {
	tests := []struct {
		name      string
		session   map[string]any
		preserved int
		wantError bool
	}{
		{name: "reachable", session: map[string]any{"id": "baseline", "pid": 123}, preserved: 1},
		{name: "older reachable response", session: map[string]any{"id": "baseline"}, preserved: 1},
		{name: "naturally exited", session: map[string]any{"id": "baseline", "pid": 123, "exited": true}},
		{name: "explicitly ended before exit", session: map[string]any{"id": "baseline", "unreachable": true, "ended_by_kind": "user"}},
		{name: "reaped but unreachable", session: map[string]any{"id": "baseline", "exited": true, "unreachable": true}},
		{name: "unknown", wantError: true},
		{name: "unreachable live runner", session: map[string]any{"id": "baseline", "pid": 123, "unreachable": true}, wantError: true},
		{name: "gone runner without exit", session: map[string]any{"id": "baseline", "pid": 123, "unreachable": true, "runnerGone": true}, wantError: true},
		{name: "retained history without exit", session: map[string]any{"id": "baseline", "pid": 0}, wantError: true},
		{name: "non-user boundary is not an exit", session: map[string]any{"id": "baseline", "pid": 123, "unreachable": true, "ended_by_kind": "agent"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("SESSIONS_STATE_DIR", filepath.Join(root, "runners"))
			t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(root, "lanes.sqlite3"))
			t.Setenv("SESSIONS_PORT", "8899")
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/api/health" {
					_, _ = io.WriteString(response, `{"ok":true,"name":"sessionsd","version":"v0.2.16"}`)
					return
				}
				var sessions []map[string]any
				if test.session != nil && (test.session["exited"] != true || request.URL.Query().Get("include_exited") == "1") {
					sessions = append(sessions, test.session)
				}
				_ = json.NewEncoder(response).Encode(map[string]any{"sessions": sessions})
			}))
			defer server.Close()
			application, err := newApp([]string{"--host", server.URL, "update"}, strings.NewReader(""), io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer application.close()
			clock := time.Unix(0, 0)
			application.now = func() time.Time { return clock }
			application.sleep = func(duration time.Duration) { clock = clock.Add(duration) }
			application.cliIsCurrent = func(string) bool { return true }
			baseline := updateConvergenceBaseline{SessionIDs: map[string]struct{}{"baseline": {}}}
			preserved, err := application.waitForUpdateConvergence(context.Background(), "0.2.16", baseline)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "still reconnecting to 1 live sessions") || !strings.Contains(err.Error(), "after 32s") {
					t.Fatalf("missing/unreachable session must remain unfinished: preserved=%d, error=%v", preserved, err)
				}
				return
			}
			if err != nil || preserved != test.preserved {
				t.Fatalf("preserved=%d, error=%v; want %d reachable sessions", preserved, err, test.preserved)
			}
		})
	}
}
