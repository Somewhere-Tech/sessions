package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// accountsDaemon answers the account routes and records what the CLI asked for.
func accountsDaemon(t *testing.T, created *map[string]string, sessionBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/profiles":
			_ = json.NewEncoder(response).Encode(map[string]any{"profiles": []any{
				map[string]any{
					"tool": "claude", "name": "work", "label": "Work — team plan", "signed_in": true,
					"path": "/state/profiles/claude/work", "sessions": []any{}, "last_used": 1757000000000,
				},
				map[string]any{
					"tool": "codex", "name": "personal", "signed_in": false,
					"path": "/state/profiles/codex/personal", "sessions": []any{}, "last_used": 0,
				},
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/profiles":
			var body map[string]string
			_ = json.NewDecoder(request.Body).Decode(&body)
			*created = body
			_ = json.NewEncoder(response).Encode(map[string]any{"profile": map[string]any{
				"tool": body["tool"], "name": body["name"], "label": body["label"],
				"path": "/state/profiles/" + body["tool"] + "/" + body["name"], "signed_in": false,
				"sessions": []any{}, "last_used": 0,
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/account-logins":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			*sessionBody = body
			_ = json.NewEncoder(response).Encode(map[string]any{"id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/profiles/"):
			_ = json.NewEncoder(response).Encode(map[string]any{
				"ok": true, "forgotten": strings.TrimPrefix(request.URL.Path, "/api/profiles/"),
				"home": "/state/profiles/claude/work",
				"note": "the provider home was left in place for manual review",
			})
		default:
			http.NotFound(response, request)
		}
	}))
}

// The list says which account is which, and the one thing Sessions can see
// about its login: whether the file a provider writes at sign-in is present.
func TestAccountsListsLabelsAndWhetherALoginFileIsThere(t *testing.T) {
	var created map[string]string
	var sessionBody map[string]any
	server := accountsDaemon(t, &created, &sessionBody)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "accounts"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("accounts exit=%d stderr=%q", code, stderr.String())
	}
	// The column says what is actually known: whether the file a provider writes
	// at sign-in is there. Not "signed in", which Sessions cannot see.
	for _, fragment := range []string{"LABEL", "LOGIN-FILE", "Work — team plan", "present", "personal", "none"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("accounts output lacks %q:\n%s", fragment, stdout.String())
		}
	}
}

// Adding an account registers the home and opens the provider's own login in a
// session, in that home, where the person can watch it.
func TestAccountsAddRegistersTheHomeAndOpensTheProvidersLogin(t *testing.T) {
	for _, test := range []struct {
		tool    string
		command string
		args    int
		hint    string
	}{
		{"claude", "claude", 0, "login-status"},
		{"codex", "codex", 0, "login-status"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			var created map[string]string
			var sessionBody map[string]any
			server := accountsDaemon(t, &created, &sessionBody)
			defer server.Close()
			t.Setenv("HOME", t.TempDir())

			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "accounts", "add", "work", "--tool", test.tool, "--label", "Work — team plan"},
				strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("accounts add exit=%d stderr=%q", code, stderr.String())
			}
			if created["tool"] != test.tool || created["name"] != "work" || created["label"] != "Work — team plan" {
				t.Fatalf("registered account = %#v", created)
			}
			if sessionBody["tool"] != test.command || sessionBody["profile"] != "work" {
				t.Fatalf("login session = %#v", sessionBody)
			}
			// No working directory from this caller: with --machine the session
			// is created on another computer, whose home is not this one's.
			if cwd, present := sessionBody["cwd"]; present {
				t.Fatalf("the login session was pinned to this caller's directory %q; the target daemon must choose its own", cwd)
			}
			if args, _ := sessionBody["args"].([]any); len(args) != test.args {
				t.Fatalf("login session args = %#v", sessionBody["args"])
			}
			if !strings.Contains(stdout.String(), test.hint) {
				t.Errorf("output does not say how to finish the login:\n%s", stdout.String())
			}
			if !strings.Contains(stdout.String(), "check the account in the browser") {
				t.Errorf("output does not ask the person to check the account:\n%s", stdout.String())
			}
			if strings.Contains(strings.ToLower(stdout.String()), "api key") {
				t.Errorf("output offers an API key login:\n%s", stdout.String())
			}
		})
	}
}

// Forgetting says what was left behind, because what is left is a real
// subscription's login and history.
func TestAccountsForgetLeavesTheHomeAndSaysWhere(t *testing.T) {
	var created map[string]string
	var sessionBody map[string]any
	server := accountsDaemon(t, &created, &sessionBody)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "accounts", "forget", "work", "--tool", "claude"},
		strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("accounts forget exit=%d stderr=%q", code, stderr.String())
	}
	for _, fragment := range []string{"no longer listed", "left in place", "/state/profiles/claude/work"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("forget output lacks %q:\n%s", fragment, stdout.String())
		}
	}
}

func TestAccountsRefusesAnIncompleteRequest(t *testing.T) {
	var created map[string]string
	var sessionBody map[string]any
	server := accountsDaemon(t, &created, &sessionBody)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"accounts", "add", "work"},
		{"accounts", "add", "work", "--tool", "shell"},
		{"accounts", "forget", "work"},
		{"accounts", "nonsense"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--host", server.URL}, args...), strings.NewReader(""), &stdout, &stderr); code == 0 {
			t.Errorf("%v was accepted", args)
		}
		if created != nil {
			t.Fatalf("%v registered an account: %#v", args, created)
		}
	}
}

// A subscription is signed into once per machine, so adding one has to be
// possible on the machine that needs it. The request goes through the local
// daemon's fleet relay — the same route every other --machine verb uses — and
// carries nothing local to this caller.
func TestAccountsAddOnAnotherMachineLandsOnThatMachine(t *testing.T) {
	var paths []string
	var sessionBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		paths = append(paths, request.Method+" "+request.URL.Path)
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/api/profiles"):
			var body map[string]string
			_ = json.NewDecoder(request.Body).Decode(&body)
			_ = json.NewEncoder(response).Encode(map[string]any{"profile": map[string]any{
				"tool": body["tool"], "name": body["name"], "label": body["label"],
				"path": "/state/profiles/" + body["tool"] + "/" + body["name"], "signed_in": false,
				"sessions": []any{}, "last_used": 0,
			}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/api/account-logins"):
			_ = json.NewDecoder(request.Body).Decode(&sessionBody)
			_ = json.NewEncoder(response).Encode(map[string]any{"id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONS_HOST", server.URL)
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mini", Endpoint: "http://10.0.0.2:8787", Transport: "nearby",
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"accounts", "add", "work", "--tool", "claude", "--label", "Work — team plan", "--machine", "mini"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("accounts add --machine exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// Both halves of the guided login landed on the machine that was named.
	want := []string{
		"POST /api/fleet/machine-mini/api/profiles",
		"POST /api/fleet/machine-mini/api/account-logins",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want them relayed to the target machine", paths)
	}
	// Nothing local to this caller travels with it. A working directory from
	// here need not exist over there, and the target daemon has its own default.
	if _, sent := sessionBody["cwd"]; sent {
		t.Fatalf("the create carried this caller's working directory: %#v", sessionBody)
	}
	if sessionBody["profile"] != "work" || sessionBody["tool"] != "claude" {
		t.Fatalf("login session = %#v, want claude in the work account's home", sessionBody)
	}
	// And it says where all this happened, so the follow-up command is right.
	for _, fragment := range []string{"on mini", "--machine mini accounts login-status"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("output lacks %q:\n%s", fragment, stdout.String())
		}
	}
}

// A machine nobody has approved is a usage error, not a silent local add.
func TestAccountsAddRefusesAnUnknownMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"accounts", "add", "work", "--tool", "claude", "--machine", "nowhere"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("accounts add accepted an unknown machine: %q", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused add wrote to stdout: %q", stdout.String())
	}
}
