package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestAddedRouteShapeTable(t *testing.T) {
	fixture := t.TempDir()
	home := filepath.Join(fixture, "home")
	mustMkdirAll(t, filepath.Join(home, "Code", "nested-project", ".git"))
	mustMkdirAll(t, filepath.Join(home, "top-project"))
	mustWriteFile(t, filepath.Join(home, "top-project", "go.mod"), []byte("module example.test/top\n"))
	mustMkdirAll(t, filepath.Join(home, "Alpha"))
	mustWriteFile(t, filepath.Join(home, "readme.txt"), []byte("fixture\n"))
	if err := os.Symlink(filepath.Join(home, "Alpha"), filepath.Join(home, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	canonicalHome := canonicalPath(home)

	projectDir := filepath.Join(home, ".claude", "projects", "-Users-fixture-Code-nested-project")
	mustMkdirAll(t, projectDir)
	jsonlPath := filepath.Join(projectDir, "session-one.jsonl")
	jsonl := []byte("{\"type\":\"assistant\",\"message\":{\"content\":\"skip\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"skip\"},{\"type\":\"text\",\"text\":\"  first\\nfixture   request  \"}]}}\n")
	mustWriteFile(t, jsonlPath, jsonl)
	modified := time.Unix(1_700_000_000, 123_000_000)
	if err := os.Chtimes(jsonlPath, modified, modified); err != nil {
		t.Fatal(err)
	}

	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: daemon.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.registry.RequestKill(context.Background(), created.ID, false) })

	tests := []struct {
		name     string
		method   string
		target   string
		body     io.Reader
		headers  http.Header
		validate func(*testing.T, map[string]any)
	}{
		{
			name:   "GET /api/directories",
			method: http.MethodGet, target: "/api/directories",
			validate: func(t *testing.T, body map[string]any) {
				assertExactKeys(t, body, "directories")
				directories := valueSlice(t, body, "directories")
				got := make(map[string]map[string]any, len(directories))
				for _, value := range directories {
					candidate := valueMap(t, value)
					assertExactKeys(t, candidate, "kind", "label", "path")
					got[candidate["path"].(string)] = candidate
				}
				for path, want := range map[string][2]string{
					home:                        {"~", "home"},
					filepath.Join(home, "Code"): {"~/Code", "common"},
					filepath.Join(home, "Code", "nested-project"): {"~/Code/nested-project", "project"},
				} {
					candidate, ok := got[path]
					if !ok || candidate["label"] != want[0] || candidate["kind"] != want[1] {
						t.Errorf("candidate %q = %#v, want label=%q kind=%q", path, candidate, want[0], want[1])
					}
				}
				if candidate := got[filepath.Join(home, "top-project")]; candidate != nil {
					t.Errorf("broad home scan unexpectedly returned %#v", candidate)
				}
				if len(directories) == 0 || valueMap(t, directories[0])["kind"] != "project" {
					t.Errorf("projects should be offered before broad folders: %#v", directories)
				}
			},
		},
		{
			name:   "GET /api/fs/list",
			method: http.MethodGet, target: "/api/fs/list?path=" + url.QueryEscape(home),
			validate: func(t *testing.T, body map[string]any) {
				assertExactKeys(t, body, "entries", "parent", "path")
				if body["path"] != canonicalHome || body["parent"] != nil {
					t.Fatalf("fs/list root = %#v", body)
				}
				entries := valueSlice(t, body, "entries")
				got := make(map[string]map[string]any, len(entries))
				seenFile := false
				for _, value := range entries {
					entry := valueMap(t, value)
					assertExactKeys(t, entry, "hidden", "kind", "name")
					kind := entry["kind"].(string)
					if kind != "dir" {
						seenFile = true
					} else if seenFile {
						t.Errorf("directory %q sorted after a non-directory", entry["name"])
					}
					got[entry["name"].(string)] = entry
				}
				for name, wantKind := range map[string]string{
					"Alpha": "dir", "linked-dir": "symlink", "readme.txt": "file", ".claude": "dir",
				} {
					entry, ok := got[name]
					if !ok || entry["kind"] != wantKind {
						t.Errorf("entry %q = %#v, want kind %q", name, entry, wantKind)
					}
				}
				if got[".claude"]["hidden"] != true || got["readme.txt"]["hidden"] != false {
					t.Errorf("hidden flags: .claude=%#v readme=%#v", got[".claude"], got["readme.txt"])
				}
			},
		},
		{
			name:   "GET /api/claude-sessions",
			method: http.MethodGet, target: "/api/claude-sessions",
			validate: func(t *testing.T, body map[string]any) {
				assertExactKeys(t, body, "sessions")
				sessions := valueSlice(t, body, "sessions")
				if len(sessions) != 1 {
					t.Fatalf("sessions = %#v", sessions)
				}
				session := valueMap(t, sessions[0])
				assertExactKeys(t, session, "cwd", "firstUserMessage", "modifiedAt", "sessionId", "sizeBytes")
				if session["sessionId"] != "session-one" ||
					session["cwd"] != "/Users/fixture/Code/nested/project" ||
					session["firstUserMessage"] != "first fixture request" ||
					session["modifiedAt"] != float64(1_700_000_000_123) ||
					session["sizeBytes"] != float64(len(jsonl)) {
					t.Fatalf("claude session shape = %#v", session)
				}
			},
		},
		{
			name:   "POST /api/sessions/:id/upload",
			method: http.MethodPost, target: "/api/sessions/" + created.ID + "/upload",
			body:    bytes.NewBufferString("uploaded fixture"),
			headers: http.Header{"X-Sessions-Filename": {"../unsafe?.png"}, "Content-Type": {"image/png"}},
			validate: func(t *testing.T, body map[string]any) {
				assertExactKeys(t, body, "path", "size")
				path, ok := body["path"].(string)
				if !ok || body["size"] != float64(len("uploaded fixture")) {
					t.Fatalf("upload response = %#v", body)
				}
				wantDir := filepath.Join(home, ".local", "state", "sessions", "uploads")
				if filepath.Dir(path) != wantDir || !regexp.MustCompile(`^unsafe_-[0-9a-f]{8}\.png$`).MatchString(filepath.Base(path)) {
					t.Fatalf("upload path = %q", path)
				}
				contents, err := os.ReadFile(path)
				if err != nil || string(contents) != "uploaded fixture" {
					t.Fatalf("uploaded contents = %q, err=%v", contents, err)
				}
				assertMode(t, path, 0o600)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serve(t, daemon.handler, test.method, test.target, test.body, "127.0.0.1:1", test.headers)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			decodeBody(t, response, &body)
			test.validate(t, body)
		})
	}
}

func TestFSListRejectsTraversal(t *testing.T) {
	fixture := t.TempDir()
	home := filepath.Join(fixture, "home")
	outside := filepath.Join(fixture, "outside")
	mustMkdirAll(t, home)
	mustMkdirAll(t, outside)
	t.Setenv("HOME", home)
	canonicalOutside := canonicalPath(outside)
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	daemon := newTestDaemon(t)

	tests := []struct {
		name string
		path string
	}{
		{name: "dot-dot escape", path: filepath.Join(home, "..", "outside")},
		{name: "symlink escape", path: filepath.Join(home, "escape")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serve(t, daemon.handler, http.MethodGet, "/api/fs/list?path="+url.QueryEscape(test.path), nil, "127.0.0.1:1", nil)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			decodeBody(t, response, &body)
			assertExactKeys(t, body, "error", "path")
			if body["error"] != "path outside home directory" || body["path"] != canonicalOutside {
				t.Fatalf("traversal response = %#v", body)
			}
		})
	}
}

func assertExactKeys(t *testing.T, value map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(value))
	for key := range value {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("keys = %v, want %v; value=%#v", got, want, value)
	}
}

func valueSlice(t *testing.T, value map[string]any, key string) []any {
	t.Helper()
	result, ok := value[key].([]any)
	if !ok {
		t.Fatalf("%q = %#v, want array", key, value[key])
	}
	return result
}

func valueMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v, want object", value)
	}
	return result
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
