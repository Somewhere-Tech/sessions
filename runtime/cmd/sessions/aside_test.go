package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAsideCommandAndListFiltersPreserveLiveCLIVisibility(t *testing.T) {
	const (
		liveID  = "24000000-0000-4000-8000-000000000001"
		otherID = "24000000-0000-4000-8000-000000000002"
		endedID = "24000000-0000-4000-8000-000000000003"
	)
	t.Setenv("HOME", t.TempDir())
	setAside := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			asideField := ""
			if setAside {
				asideField = `,"setAsideAt":1700000000000`
			}
			_, _ = response.Write([]byte(`{"sessions":[` +
				`{"id":"` + liveID + `","name":"fast","description":"","cmd":"codex","cwd":"/tmp","createdAt":1,"pid":12,"tool":"codex","working":false,"lastDataAt":1,"lastUserMessageAt":null,"exited":false` + asideField + `},` +
				`{"id":"` + otherID + `","name":"current","description":"","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":13,"tool":"claude-code","working":true,"lastDataAt":1,"lastUserMessageAt":null,"exited":false},` +
				`{"id":"` + endedID + `","name":"old","description":"","cmd":"claude","cwd":"/tmp","createdAt":1,"pid":0,"tool":"claude-code","working":false,"lastDataAt":1,"lastUserMessageAt":null,"exited":true,"exitCode":0,"setAsideAt":1600000000000}` +
				`]}`))
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+liveID+"/set-aside":
			var body struct {
				SetAside bool `json:"setAside"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode aside body: %v", err)
			}
			setAside = body.SetAside
			if setAside {
				_, _ = response.Write([]byte(`{"setAsideAt":1700000000000}`))
			} else {
				_, _ = response.Write([]byte(`{"setAsideAt":null}`))
			}
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+endedID+"/set-aside":
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":"this session has ended; use archive to hide an ended record"}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/verdict"):
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"not found"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "aside", liveID, endedID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "set-aside") ||
		!strings.Contains(stdout, "skipped") || !strings.Contains(stdout, "use archive") || !setAside {
		t.Fatalf("aside exit=%d stdout=%q stderr=%q setAside=%v", code, stdout, stderr, setAside)
	}

	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "fast") || !strings.Contains(stdout, "set-aside") {
		t.Fatalf("ls hides set-aside session exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls", "--aside")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "fast") || strings.Contains(stdout, "current") {
		t.Fatalf("ls --aside exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls", "--not-aside")
	if code != 0 || stderr != "" || strings.Contains(stdout, "fast") || !strings.Contains(stdout, "current") {
		t.Fatalf("ls --not-aside exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "ls", "--aside")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"setAsideAt": 1700000000000`) {
		t.Fatalf("json ls --aside exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "status", liveID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "set aside") ||
		!strings.Contains(stdout, "runtime is running") {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "status", endedID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "was set aside") ||
		strings.Contains(stdout, "runtime is running") {
		t.Fatalf("ended status exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = runOwnershipCLI(t, server.URL, "aside", liveID, "--clear")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "brought-back") || setAside {
		t.Fatalf("aside --clear exit=%d stdout=%q stderr=%q setAside=%v", code, stdout, stderr, setAside)
	}
}

func TestAsideListFiltersAreMutuallyExclusive(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"ls", "--aside", "--not-aside"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
