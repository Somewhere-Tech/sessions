package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsageCommandForwardsFiltersAndPrintsTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/usage" || request.URL.Query().Get("group") != "tag" ||
			request.URL.Query().Get("dimension") != "product" || request.URL.Query().Get("provider") != "claude" ||
			request.URL.Query().Get("mode") != "calculate" {
			http.Error(response, request.URL.String(), http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"schemaVersion":1,"machine":"test-mac","group":"tag","mode":"calculate","rows":[{"key":"Sessions","models":["claude-sonnet-4-6"],"tokens":{"inputTokens":1200,"outputTokens":300,"cacheCreationTokens":100,"cacheReadTokens":500,"reasoningTokens":75},"costUSD":0.25,"entries":2}],"totals":{"key":"total","models":["claude-sonnet-4-6"],"tokens":{"inputTokens":1200,"outputTokens":300,"cacheCreationTokens":100,"cacheReadTokens":500,"reasoningTokens":75},"costUSD":0.25,"entries":2}}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "usage", "tag", "--dimension", "product", "--provider", "claude", "--mode", "calculate"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Sessions") || !strings.Contains(stdout.String(), "REASONING") || !strings.Contains(stdout.String(), "75") || !strings.Contains(stdout.String(), "2,100") || !strings.Contains(stdout.String(), "$0.2500") {
		t.Fatalf("usage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestUsageCommandRejectsTagWithoutDimension(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usage", "tag"}, strings.NewReader(""), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--dimension") {
		t.Fatalf("usage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
