package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnboardingStatusIsReadOnlyAndHasJSONParity(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount++
		if request.URL.Path != "/api/onboarding" || request.Method != http.MethodGet {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"version":1,"complete":false,"remoteControl":"pending"}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "onboarding"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("plain exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Onboarding: pending") ||
		!strings.Contains(stdout.String(), "cannot grant consent") {
		t.Fatalf("plain output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "--json", "onboarding"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got onboardingStatus
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("json output = %q: %v", stdout.String(), err)
	}
	if got.Version != 1 || got.Complete || got.RemoteControl != "pending" {
		t.Fatalf("json output = %#v", got)
	}
	if requestCount != 2 {
		t.Fatalf("request count = %d", requestCount)
	}
}
