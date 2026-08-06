package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An agent pipes `sessions --json ... | jq`. If a failure arrives as prose on
// stderr, the decoder sees empty input and the loop dies at exactly the moment
// something went wrong, with no machine-readable reason.
func TestJSONModeReportsFailuresAsJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "definitely-not-a-command"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatal("unknown command exited 0")
	}
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatalf("--json failure was not decodable: %v (stdout %q stderr %q)",
			err, stdout.String(), stderr.String())
	}
	if failure.OK {
		t.Fatal("failure reported ok:true")
	}
	if failure.Error == "" {
		t.Fatal("failure carried no reason")
	}
	if failure.Code != code {
		t.Fatalf("code field = %d but process exited %d; they must agree", failure.Code, code)
	}
}

// Without --json the failure stays prose on stderr, and stdout stays clean for
// whatever the caller was piping.
func TestPlainModeKeepsFailuresOnStderr(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"definitely-not-a-command"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("unknown command exited 0")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want it left clean", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q, want an instructional message", stderr.String())
	}
}

// Under --json, stdout carries exactly one JSON document on every path. Some
// commands emit a structured report and then exit non-zero; appending a
// synthesized error object after such a report would put two values on one
// stream, which no decoder accepts, so the report is left to stand alone.
func TestJSONFailureEmitsExactlyOneDocument(t *testing.T) {
	id := "24000000-0000-4000-8000-00000000009a"
	// The daemon knows of no such session, so kill reports a per-target
	// failure item and then exits non-zero -- the report-then-fail shape.
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[]}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/lanes":
			_, _ = response.Write([]byte(`{"lanes":[]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "--json", "kill", id},
		strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("a refused kill exited 0")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var first any
	if err := decoder.Decode(&first); err != nil {
		t.Fatalf("no decodable report on stdout: %v (%q)", err, stdout.String())
	}
	var second any
	if err := decoder.Decode(&second); err == nil {
		t.Fatalf("stdout carried a second JSON document: %q", stdout.String())
	}
	// Whichever document it is, it has to explain itself: the caller under
	// --json is not reading stderr.
	explained := strings.Contains(stdout.String(), "reason") ||
		strings.Contains(stdout.String(), "error")
	if !explained {
		t.Fatalf("the document did not explain the failure: %q", stdout.String())
	}
}
