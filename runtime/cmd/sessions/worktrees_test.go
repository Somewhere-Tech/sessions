package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeFlagsFlowThroughNewAndRunWithoutConsumingChildArgs(t *testing.T) {
	cwd := t.TempDir()
	tests := []struct {
		name     string
		path     string
		args     []string
		wantBase string
		wantArgs []any
	}{
		{
			name: "new", path: "/api/sessions", wantBase: "main",
			args: []string{"new", "--cmd", "/bin/sh", "--name", "fix", "--cwd", cwd, "--worktree", "--base", "main"},
		},
		{
			name: "run", path: "/api/lanes", wantBase: "release", wantArgs: []any{"--base", "child-value"},
			args: []string{"run", "--name", "lane", "--cwd", cwd, "--worktree", "--base", "release", "--", "/bin/sh", "--base", "child-value"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var posted map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != test.path {
					http.NotFound(response, request)
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(response, `{"id":"worktree-test"}`)
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			var stdout, stderr bytes.Buffer
			arguments := append([]string{"--host", server.URL}, test.args...)
			if code := run(arguments, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if posted["worktree"] != true || posted["base"] != test.wantBase || posted["cwd"] != filepath.Clean(cwd) {
				t.Fatalf("posted worktree request = %#v", posted)
			}
			if test.wantArgs != nil {
				args, _ := posted["args"].([]any)
				if len(args) != len(test.wantArgs) || args[0] != test.wantArgs[0] || args[1] != test.wantArgs[1] {
					t.Fatalf("child args were changed: %#v", posted["args"])
				}
			}
		})
	}
}

func TestWorktreeBaseRequiresWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"new", "--cmd", "/bin/sh", "--base", "main"}, strings.NewReader(""), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "--base requires --worktree") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestWorktreesListAndCleanCLIJSONAndHumanPlan(t *testing.T) {
	postedDryRun := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		item := map[string]any{
			"session": "10000000-0000-4000-8000-000000000001", "session_name": "parser",
			"worktree_path": "/tmp/project-wt/parser", "branch": "sessions/parser", "base": "main",
			"source_repo": "/tmp/project", "tree_state": "clean", "dirty": false,
			"merged_into_base": true, "session_state": "exited", "exists": true,
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/worktrees":
			_ = json.NewEncoder(response).Encode(map[string]any{"worktrees": []any{item}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/worktrees/clean":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode clean request: %v", err)
				return
			}
			postedDryRun, _ = body["dry_run"].(bool)
			item["action"] = "would-remove"
			_ = json.NewEncoder(response).Encode(map[string]any{"results": []any{item}, "dry_run": postedDryRun})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	stdout, stderr, code := runWorktreeCLI(t, server.URL, "--json", "worktrees")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"worktree_path": "/tmp/project-wt/parser"`) {
		t.Fatalf("JSON list exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runWorktreeCLI(t, server.URL, "worktrees", "clean", "--dry-run")
	if code != 0 || stderr != "" || !postedDryRun || !strings.Contains(stdout, "would remove /tmp/project-wt/parser (sessions/parser)") {
		t.Fatalf("dry-run exit=%d posted=%v stdout=%q stderr=%q", code, postedDryRun, stdout, stderr)
	}
}

func TestWorktreeProvenanceShowsInSessionsAndStatusJSON(t *testing.T) {
	cwd := t.TempDir()
	const id = "20000000-0000-4000-8000-000000000001"
	session := map[string]any{
		"id": id, "name": "worktree", "description": "test", "cmd": "/bin/sh", "args": []any{},
		"cwd": cwd, "worktree_path": cwd, "branch": "sessions/worktree", "base": "main", "source_repo": filepath.Dir(cwd),
		"cols": 80, "rows": 24, "createdAt": 1000, "pid": 1, "tool": "terminal", "working": false,
		"lastDataAt": 1000, "lastUserMessageAt": nil, "exited": false, "exitCode": nil, "exitSignal": nil, "exitedAt": nil,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{session}})
		case "/api/sessions/" + id + "/verdict":
			response.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(response, `{"error":"not found"}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	for _, command := range [][]string{{"--json", "list", "--include-closed"}, {"--json", "status", id}} {
		stdout, stderr, code := runWorktreeCLI(t, server.URL, command...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v exit=%d stdout=%q stderr=%q", command, code, stdout, stderr)
		}
		for _, field := range []string{`"worktree_path"`, `"branch": "sessions/worktree"`, `"base": "main"`, `"source_repo"`} {
			if !strings.Contains(stdout, field) {
				t.Fatalf("%v output missing %s: %s", command, field, stdout)
			}
		}
	}
}

func runWorktreeCLI(t *testing.T, host string, args ...string) (string, string, int) {
	t.Helper()
	arguments := append([]string{"--host", host}, args...)
	var stdout, stderr bytes.Buffer
	code := run(arguments, strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}
