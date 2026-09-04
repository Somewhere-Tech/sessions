package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWaitRestartRecognizesOnlyReconnectableTransportFailures(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET} {
		if !isRestartTransportError(fmt.Errorf("request failed: %w", err)) {
			t.Fatalf("%v was not reconnectable", err)
		}
	}
	if isRestartTransportError(errors.New("decode session list: invalid JSON")) {
		t.Fatal("a protocol error was treated as a daemon restart")
	}
}

func closeRequestConnection(t *testing.T, response http.ResponseWriter) {
	t.Helper()
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		t.Fatal("test server cannot close its connection")
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack connection: %v", err)
	}
	_ = connection.Close()
}

func TestWaitRetriesEOFAcrossDaemonRestartAndAnnouncesOnce(t *testing.T) {
	id := "71000000-0000-4000-8000-000000000001"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		calls++
		if calls == 2 || calls == 3 {
			closeRequestConnection(t, response)
			return
		}
		working := calls == 1
		idleReason := "completed"
		if working {
			idleReason = ""
		}
		_ = json.NewEncoder(response).Encode(sessionsResponse{Sessions: []session{{
			ID: id, Cmd: "codex", Cwd: "/work", Working: working, IdleReason: idleReason,
		}}})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "wait", id[:8], "--timeout", "5s"},
		strings.NewReader(""), &stdout, &stderr)
	if code != exitSatisfied {
		t.Fatalf("wait exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Count(stderr.String(), waitRestartNotice) != 1 {
		t.Fatalf("restart notice = %q, want exactly once", stderr.String())
	}
}

func TestFanoutJoinRetriesTransportOutageUntilDaemonReturns(t *testing.T) {
	id := "72000000-0000-4000-8000-000000000001"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/sessions" {
			http.NotFound(response, request)
			return
		}
		calls++
		if calls == 1 {
			closeRequestConnection(t, response)
			return
		}
		_ = json.NewEncoder(response).Encode(sessionsResponse{Sessions: []session{{
			ID: id, Cmd: "codex", Cwd: "/work", IdleReason: "completed",
		}}})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	results, _, err := application.runWaitJoin(
		[]waitTargetRef{{id: id}}, 0, 5*time.Second, true, false,
	)
	if err != nil || len(results) != 1 || !results[0].OK {
		t.Fatalf("join results=%+v err=%v stderr=%q", results, err, stderr.String())
	}
	if strings.Count(stderr.String(), waitRestartNotice) != 1 {
		t.Fatalf("restart notice = %q, want exactly once", stderr.String())
	}
}

func TestLaneWaitRetriesDaemonRestartBeforeCompletion(t *testing.T) {
	id := "73000000-0000-4000-8000-000000000001"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if calls == 1 {
			closeRequestConnection(t, response)
			return
		}
		if calls == 2 {
			response.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewEncoder(response).Encode(laneManifest{ExitCode: 0, DurationMS: 250})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	application, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	completed, manifest, err := application.waitForLaneExit([]string{id}, 5*time.Second)
	if err != nil || completed != id || manifest.ExitCode != 0 {
		t.Fatalf("lane wait id=%q manifest=%+v err=%v", completed, manifest, err)
	}
	if strings.Count(stderr.String(), waitRestartNotice) != 1 {
		t.Fatalf("restart notice = %q, want exactly once", stderr.String())
	}
}
