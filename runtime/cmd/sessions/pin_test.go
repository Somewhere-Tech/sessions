package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pinFixtureDaemon answers the three routes pin, unpin, and the list surfaces
// need, and keeps the pin state so the CLI's own read-back is exercised rather
// than assumed.
func pinFixtureDaemon(t *testing.T, pinnedID, plainID string, pinned *bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			// The unpinned session is listed first on purpose: if the CLI does
			// not sort, the pinned one stays second and the test says so.
			_, _ = response.Write([]byte(`{"sessions":[` +
				`{"id":"` + plainID + `","name":"scratch","description":"","cmd":"claude","cwd":"/tmp",` +
				`"createdAt":1,"pid":13,"tool":"claude-code","working":false,"lastDataAt":1,` +
				`"lastUserMessageAt":null,"exited":false,"pinned":false},` +
				`{"id":"` + pinnedID + `","name":"bolo","description":"","cmd":"codex","cwd":"/tmp",` +
				`"createdAt":1,"pid":12,"tool":"codex","working":false,"lastDataAt":1,` +
				`"lastUserMessageAt":null,"exited":false,"pinned":` + boolText(*pinned) + `}` +
				`]}`))
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+pinnedID+"/pin":
			var body struct {
				Pinned bool `json:"pinned"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode pin body: %v", err)
			}
			*pinned = body.Pinned
			_, _ = response.Write([]byte(`{"pinned":` + boolText(*pinned) + `}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/verdict"):
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"not found"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestPinAndUnpinPersistThroughTheDaemonAndSurfaceInListings(t *testing.T) {
	const (
		pinnedID = "31000000-0000-4000-8000-000000000001"
		plainID  = "32000000-0000-4000-8000-000000000002"
	)
	t.Setenv("HOME", t.TempDir())
	pinned := false
	server := pinFixtureDaemon(t, pinnedID, plainID, &pinned)

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "pin", pinnedID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "pinned") || !pinned {
		t.Fatalf("pin exit=%d stdout=%q stderr=%q pinned=%v", code, stdout, stderr, pinned)
	}
	if !strings.Contains(stdout, "bolo") {
		t.Fatalf("pin did not name the session it marked: %q", stdout)
	}

	// A prefix is what a user actually types after reading `sessions ls`.
	pinned = false
	if stdout, stderr, code = runOwnershipCLI(t, server.URL, "pin", pinnedID[:8]); code != 0 || stderr != "" || !pinned {
		t.Fatalf("pin by prefix exit=%d stdout=%q stderr=%q pinned=%v", code, stdout, stderr, pinned)
	}

	stdout, stderr, code = runOwnershipCLI(t, server.URL, "unpin", pinnedID)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "unpinned") || pinned {
		t.Fatalf("unpin exit=%d stdout=%q stderr=%q pinned=%v", code, stdout, stderr, pinned)
	}
}

// `sessions help` promises that every --json document carries a code matching
// the exit status. An agent that pins a session and reads a missing code as
// zero would be right by accident here and wrong the moment the call fails, so
// the field is asserted on the success path where an omitempty would erase it.
func TestPinJSONCarriesOKAndACodeMatchingTheExit(t *testing.T) {
	const (
		pinnedID = "31000000-0000-4000-8000-000000000003"
		plainID  = "32000000-0000-4000-8000-000000000004"
	)
	t.Setenv("HOME", t.TempDir())
	pinned := false
	server := pinFixtureDaemon(t, pinnedID, plainID, &pinned)

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "--json", "pin", pinnedID)
	if code != 0 || stderr != "" {
		t.Fatalf("json pin exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertCodeMatchesExit(t, "pin", code, stdout)
	var result pinResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("undecodable pin document: %v (%q)", err, stdout)
	}
	if !result.OK || !result.Pinned || result.ID != pinnedID || result.Name != "bolo" {
		t.Fatalf("pin document = %+v", result)
	}

	stdout, stderr, code = runOwnershipCLI(t, server.URL, "--json", "unpin", pinnedID)
	if code != 0 || stderr != "" {
		t.Fatalf("json unpin exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertCodeMatchesExit(t, "unpin", code, stdout)
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("undecodable unpin document: %v (%q)", err, stdout)
	}
	if !result.OK || result.Pinned {
		t.Fatalf("unpin document = %+v", result)
	}
}

// A refused pin still has to answer in the format the caller asked for, with a
// code that matches the exit status.
func TestPinReportsADaemonRefusalAsJSONWithACode(t *testing.T) {
	const id = "31000000-0000-4000-8000-000000000005"
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + id + `","name":"old","description":"",` +
				`"cmd":"claude","cwd":"/tmp","createdAt":1,"pid":0,"tool":"claude-code","working":false,` +
				`"lastDataAt":1,"lastUserMessageAt":null,"exited":true,"exitCode":0,"pinned":false}]}`))
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+id+"/pin":
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":"session has ended; use archive to hide an ended record"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, _, code := runOwnershipCLI(t, server.URL, "--json", "pin", id)
	if code == 0 {
		t.Fatalf("a refused pin exited 0: %q", stdout)
	}
	assertCodeMatchesExit(t, "refused pin", code, stdout)
	if !strings.Contains(stdout, "use archive") {
		t.Fatalf("the refusal did not carry the daemon's reason: %q", stdout)
	}
}

// The listing is what the mark is for. A pinned session has to reach the top of
// both list surfaces and both formats, and the PIN column has to appear only
// when it says something.
func TestListingsPutPinnedSessionsFirst(t *testing.T) {
	const (
		pinnedID = "31000000-0000-4000-8000-000000000006"
		plainID  = "32000000-0000-4000-8000-000000000007"
	)
	t.Setenv("HOME", t.TempDir())
	pinned := true
	server := pinFixtureDaemon(t, pinnedID, plainID, &pinned)

	for _, command := range []string{"ls", "list"} {
		stdout, stderr, code := runOwnershipCLI(t, server.URL, command)
		if code != 0 || stderr != "" {
			t.Fatalf("%s exit=%d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "PIN") {
			t.Fatalf("%s did not mark the pinned session: %q", command, stdout)
		}
		if strings.Index(stdout, "bolo") > strings.Index(stdout, "scratch") {
			t.Fatalf("%s left the pinned session below the unpinned one, which is the "+
				"whole reason the mark exists: %q", command, stdout)
		}
	}

	// --json is the surface an agent reads, and it takes the head of the array.
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "--json", "ls")
	if code != 0 || stderr != "" {
		t.Fatalf("json ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var rows []struct {
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("undecodable ls document: %v (%q)", err, stdout)
	}
	if len(rows) != 2 || rows[0].ID != pinnedID || !rows[0].Pinned || rows[1].Pinned {
		t.Fatalf("json ls rows = %+v", rows)
	}

	// With nothing pinned the column is not there at all: a dash on every row
	// is noise on every row.
	pinned = false
	stdout, stderr, code = runOwnershipCLI(t, server.URL, "ls")
	if code != 0 || stderr != "" {
		t.Fatalf("unpinned ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "PIN") {
		t.Fatalf("the PIN column appeared with nothing pinned: %q", stdout)
	}
}

func TestPinRefusesAnAmbiguousInvocation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{{"pin"}, {"unpin"}, {"pin", "--off"}} {
		var stdout, stderr strings.Builder
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "usage: sessions") {
			t.Errorf("%v exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}
