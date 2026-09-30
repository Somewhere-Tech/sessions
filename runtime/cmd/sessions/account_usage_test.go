package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func usageDaemon(t *testing.T, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/account-usage" || request.Method != http.MethodGet {
			http.NotFound(response, request)
			return
		}
		*queries = append(*queries, request.URL.RawQuery)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"accounts": []any{
			map[string]any{
				"tool": "codex", "name": "work", "label": "Work", "state": "available", "checked_at": 1790000000000, "read_at": 1790000000000,
				"buckets": []any{
					map[string]any{"limit_id": "codex", "windows": []any{
						map[string]any{"kind": "primary", "used_percent": 12, "window_minutes": 300, "resets_at": 1790018000000},
						map[string]any{"kind": "secondary", "used_percent": 40},
					}},
					map[string]any{"limit_id": "codex_other", "windows": []any{map[string]any{"kind": "primary", "used_percent": 3}}},
				},
			},
			map[string]any{
				"tool": "codex", "name": "old", "state": "unavailable", "stale": true, "read_at": 1789990000000,
				"message": "Codex did not start. Refresh to try again.",
				"buckets": []any{map[string]any{"limit_id": "codex", "windows": []any{map[string]any{"kind": "primary", "used_percent": 70}}}},
			},
			map[string]any{"tool": "claude", "name": "home", "state": "unsupported", "message": "Claude usage is not connected in Sessions yet. Check Claude for your current limits."},
		}})
	}))
}

// Every limit is its own row, a stale reading says so, and an unsupported
// provider says why instead of showing an empty allowance.
func TestAccountsUsageShowsEachLimitAndItsFreshness(t *testing.T) {
	var queries []string
	server := usageDaemon(t, &queries)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "accounts", "usage"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, fragment := range []string{"codex/work", "12%", "40%", "5h0m0s", "codex_other", "3%", "unavailable (stale)", "70%", "unsupported", "not connected in Sessions yet", "never added"} {
		if !strings.Contains(out, fragment) {
			t.Errorf("usage output lacks %q:\n%s", fragment, out)
		}
	}
	if len(queries) != 1 || queries[0] != "" {
		t.Fatalf("queries = %q, want a plain read", queries)
	}
}

func TestAccountsUsageJSONAndRefreshOneAccount(t *testing.T) {
	var queries []string
	server := usageDaemon(t, &queries)
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	args := []string{"--host", server.URL, "accounts", "usage", "work", "--tool", "codex", "--refresh", "--json"}
	if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	var accounts []accountUsage
	if err := json.Unmarshal(stdout.Bytes(), &accounts); err != nil || len(accounts) != 3 || len(accounts[0].Buckets) != 2 || !accounts[1].Stale {
		t.Fatalf("json = %s (%v)", stdout.String(), err)
	}
	if len(queries) != 1 || queries[0] != "name=work&refresh=1&tool=codex" {
		t.Fatalf("queries = %q", queries)
	}
	stdout.Reset()
	if code := run([]string{"--host", server.URL, "accounts", "usage", "work"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("a name without --tool was accepted")
	}
}
