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
	if code != 0 || !strings.Contains(stdout.String(), "Sessions") || !strings.Contains(stdout.String(), "REASONING") || !strings.Contains(stdout.String(), "75") || !strings.Contains(stdout.String(), "2,100") || !strings.Contains(stdout.String(), "$0.25") {
		t.Fatalf("usage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// The money column is modelled, not measured: prices are pinned in the build
// with no as-of date, server-side tool use never reaches the token stream, and
// a subscription makes the marginal cost zero. Printing $0.2500 under a bare
// COST header asserted a precision and an authority the number does not have.
func TestUsageTablePresentsCostAsAnEstimate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"schemaVersion":1,"machine":"test-mac","group":"daily","mode":"auto","pricing":{"source":"ccusage","revision":"abc123","url":"https://example.invalid","note":"pinned snapshot; costs are estimates"},"rows":[{"key":"2026-08-01","models":["claude-sonnet-4-6"],"tokens":{"inputTokens":10,"outputTokens":10},"costUSD":1.23456,"entries":1},{"key":"2026-08-02","models":["claude-sonnet-4-6"],"tokens":{"inputTokens":1,"outputTokens":1},"costUSD":0.0001,"entries":1}],"totals":{"key":"total","models":["claude-sonnet-4-6"],"tokens":{"inputTokens":11,"outputTokens":11},"costUSD":1.23466,"entries":2}}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "usage"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("usage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	printed := stdout.String()
	for _, want := range []string{
		"EST COST",
		"$1.23",
		// A cost too small to survive rounding says so instead of reading as
		// free.
		"<$0.01",
		"modelled estimate, not a bill",
		"server-side tool use is billed but never appears",
		"Max or ChatGPT subscription",
		// The provenance the JSON report already carried now reaches the table.
		"pinned snapshot; costs are estimates",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("usage table is missing %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "$1.2346") || strings.Contains(printed, "$0.0001") {
		t.Fatalf("usage table still prints an estimate to four decimals:\n%s", printed)
	}
}

func TestFormatEstimatedCostRoundsToCentsAndNeverHidesASmallCharge(t *testing.T) {
	cases := map[float64]string{
		0:       "$0.00",
		0.0001:  "<$0.01",
		0.00499: "<$0.01",
		0.005:   "$0.01",
		1.23456: "$1.23",
		1.239:   "$1.24",
		// Thousands are grouped like every other number in the table.
		1155.239: "$1,155.24",
	}
	for value, want := range cases {
		if got := formatEstimatedCost(value); got != want {
			t.Fatalf("formatEstimatedCost(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestUsageCommandRejectsTagWithoutDimension(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usage", "tag"}, strings.NewReader(""), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--dimension") {
		t.Fatalf("usage exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
