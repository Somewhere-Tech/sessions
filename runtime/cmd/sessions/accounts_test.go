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
		case request.Method == http.MethodPost && request.URL.Path == "/api/sessions":
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

// The list says which account is which and whether its provider has signed in.
func TestAccountsListsLabelsAndSignInState(t *testing.T) {
	var created map[string]string
	var sessionBody map[string]any
	server := accountsDaemon(t, &created, &sessionBody)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "accounts"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("accounts exit=%d stderr=%q", code, stderr.String())
	}
	for _, fragment := range []string{"LABEL", "SIGNED-IN", "Work — team plan", "yes", "personal", "no"} {
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
		{"claude", "claude", 0, "/login"},
		{"codex", "codex", 1, "Sign in with ChatGPT"},
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
			if sessionBody["cmd"] != test.command || sessionBody["profile"] != "work" {
				t.Fatalf("login session = %#v", sessionBody)
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
		{"accounts", "add", "--tool", "claude"},
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
