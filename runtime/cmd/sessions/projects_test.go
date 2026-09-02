package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectsNameReportsSuggestionFailure(t *testing.T) {
	putCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/api/projects/suggest" {
			response.WriteHeader(http.StatusInternalServerError)
			_, _ = response.Write([]byte(`{"error":"Sessions could not suggest this project: read projects: permission denied"}`))
			return
		}
		if request.Method == http.MethodPut {
			putCalled = true
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	folder := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "projects", "name", folder, "Work"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "could not name this project: Sessions could not suggest this project: read projects: permission denied") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if putCalled {
		t.Fatal("CLI tried to save a project without a trustworthy suggestion")
	}
}
