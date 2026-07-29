package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSupportPreviewIsLocalRedactedAndExplicit(t *testing.T) {
	const secret = "secret-token-that-must-not-appear"
	const sessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/health" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"ok": true,
			"version": "v0.2.4",
			"discovering": false,
			"sessionsLoaded": 3,
			"token": "`+secret+`",
			"sessionId": "`+sessionID+`",
			"path": "/Users/private/work"
		}`)
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var stdout, stderr bytes.Buffer
	application, err := newApp(
		[]string{"--json", "--host", server.URL, "support", "--diagnostics"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.now = func() time.Time { return time.Date(2026, 7, 23, 20, 0, 0, 0, time.UTC) }
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	application.close()

	var preview supportPreview
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatalf("decode support preview: %v\n%s", err, stdout.String())
	}
	if preview.Uploaded || preview.Diagnostics == nil {
		t.Fatalf("preview = %+v, want local diagnostics and uploaded=false", preview)
	}
	if preview.Agent.MachineReadableCommand != "sessions --json support --diagnostics" ||
		preview.Agent.AttachmentCommand != "sessions --json support --attach --ticket tsk_ID --project somewhere-project" ||
		!preview.Agent.UserApprovalRequired ||
		preview.Agent.AutomaticSubmission ||
		len(preview.Agent.Capture) != 4 {
		t.Fatalf("agent contract = %+v", preview.Agent)
	}
	if !preview.Diagnostics.Daemon.Reachable || preview.Diagnostics.Daemon.SessionsLoaded != 3 {
		t.Fatalf("daemon preview = %+v", preview.Diagnostics.Daemon)
	}
	if preview.Diagnostics.GeneratedAt != "2026-07-23T20:00:00Z" {
		t.Fatalf("generated_at = %q", preview.Diagnostics.GeneratedAt)
	}
	encoded := stdout.String()
	for _, forbidden := range []string{secret, sessionID, home, "/Users/private/work", server.URL} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("support preview leaked %q:\n%s", forbidden, encoded)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSupportWorksWhenDaemonIsUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--json", "--host", "127.0.0.1", "--port", "1", "support", "--diagnostics"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var preview supportPreview
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Diagnostics == nil || preview.Diagnostics.Daemon.Reachable {
		t.Fatalf("daemon preview = %+v, want unreachable", preview.Diagnostics)
	}
}

func TestSupportDefaultDoesNotProbeDiagnostics(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(response, "should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--host", server.URL, "support"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if code != 0 || stderr.Len() != 0 || requests != 0 {
		t.Fatalf("exit=%d requests=%d stdout=%q stderr=%q", code, requests, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), supportTicketURL) ||
		!strings.Contains(stdout.String(), "Nothing is uploaded automatically") ||
		!strings.Contains(stdout.String(), "Agents: run `sessions --json support --diagnostics`") ||
		!strings.Contains(stdout.String(), "ask the user before opening or submitting a ticket") {
		t.Fatalf("support output = %q", stdout.String())
	}
}

func TestSupportRejectsUnknownOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"support", "--send"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), supportUsage) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSupportWritesRedactedLocalBundleWithoutOverwriting(t *testing.T) {
	const secret = "do-not-copy-this-token"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"ok": true,
			"version": "v0.2.7",
			"discovering": false,
			"sessionsLoaded": 4,
			"token": "`+secret+`",
			"path": "/private/work"
		}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	bundlePath := filepath.Join(t.TempDir(), "sessions-support.json")

	var stdout, stderr bytes.Buffer
	application, err := newApp(
		[]string{"--json", "--host", server.URL, "support", "--bundle", bundlePath},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.now = func() time.Time { return time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC) }
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	application.close()

	var receipt supportBundleReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt: %v\n%s", err, stdout.String())
	}
	if receipt.Uploaded || receipt.Path != bundlePath || receipt.SizeBytes <= 0 ||
		len(receipt.SHA256) != 64 {
		t.Fatalf("bundle receipt = %+v", receipt)
	}
	encoded, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	var bundle supportBundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("decode bundle: %v\n%s", err, encoded)
	}
	if bundle.Kind != "sessions_support_diagnostics" ||
		bundle.Diagnostics.GeneratedAt != "2026-07-29T09:30:00Z" ||
		bundle.Diagnostics.Daemon.SessionsLoaded != 4 {
		t.Fatalf("bundle = %+v", bundle)
	}
	for _, forbidden := range []string{secret, "/private/work", server.URL} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("support bundle leaked %q:\n%s", forbidden, encoded)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("bundle mode = %o, want 600", info.Mode().Perm())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code := run(
		[]string{"support", "--bundle", bundlePath},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if code != 1 || !strings.Contains(stderr.String(), "file exists") {
		t.Fatalf("overwrite attempt: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSupportAttachRequiresExplicitTicketAndUsesRedactedBundle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{
			"ok": true,
			"version": "v0.2.7",
			"discovering": false,
			"sessionsLoaded": 2,
			"command": "private provider command"
		}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	application, err := newApp(
		[]string{
			"--json", "--host", server.URL,
			"support", "--attach",
			"--ticket", "tsk_12345678",
			"--project", "sessions",
		},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.now = func() time.Time { return time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC) }
	called := 0
	application.attachSupport = func(
		ctx context.Context,
		request supportAttachmentRequest,
	) (supportAttachmentReceipt, error) {
		called++
		if request.Project != "sessions" || request.Ticket != "tsk_12345678" {
			t.Fatalf("attachment request = %+v", request)
		}
		if strings.Contains(string(request.Bundle), "private provider command") {
			t.Fatalf("attachment leaked ignored health field:\n%s", request.Bundle)
		}
		return supportAttachmentReceipt{
			SchemaVersion: 1,
			Project:       request.Project,
			Ticket:        request.Ticket,
			Path:          "/support/tsk_12345678/sessions-support-test.json",
			SHA256:        request.SHA256,
			SizeBytes:     len(request.Bundle),
			Uploaded:      true,
		}, nil
	}
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	application.close()
	if called != 1 || stderr.Len() != 0 {
		t.Fatalf("called=%d stderr=%q", called, stderr.String())
	}
	var receipt supportAttachmentReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt: %v\n%s", err, stdout.String())
	}
	if !receipt.Uploaded || receipt.Ticket != "tsk_12345678" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestSupportAttachmentUsesSomewhereCLIWithoutEnvironmentOrBundleArguments(t *testing.T) {
	bundle := []byte("{\"schema_version\":1,\"safe\":true}\n")
	sum := sha256.Sum256(bundle)
	request := supportAttachmentRequest{
		Project: "sessions",
		Ticket:  "tsk_12345678",
		Bundle:  bundle,
		SHA256:  fmt.Sprintf("%x", sum),
	}
	var stagedScriptPath string
	runner := func(
		ctx context.Context,
		name string,
		args ...string,
	) ([]byte, []byte, error) {
		if name != "somewhere" {
			t.Fatalf("command = %q", name)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, string(bundle)) || strings.Contains(joined, "--include-env") {
			t.Fatalf("unsafe command arguments = %q", joined)
		}
		if len(args) != 7 || args[0] != "run" || args[1] != "--project" ||
			args[2] != "sessions" || args[3] != "--timeout" ||
			args[4] != "10000" || args[5] != "--json" {
			t.Fatalf("command arguments = %#v", args)
		}
		stagedScriptPath = args[6]
		script, err := os.ReadFile(stagedScriptPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			"sw.tasks.get(ticket)",
			"sw.fs.write(path, bytes",
			"sw.tasks.update(ticket",
			"sw.fs.delete(path)",
			strconv.Quote(string(bundle)),
		} {
			if !strings.Contains(string(script), required) {
				t.Fatalf("staged script omitted %q:\n%s", required, script)
			}
		}
		remotePath := "/support/tsk_12345678/sessions-support-" + request.SHA256[:16] + ".json"
		receipt := supportAttachmentReceipt{
			SchemaVersion: 1,
			Project:       request.Project,
			Ticket:        request.Ticket,
			Path:          remotePath,
			SHA256:        request.SHA256,
			SizeBytes:     len(bundle),
			Uploaded:      true,
		}
		envelope, err := json.Marshal(map[string]any{"result": receipt, "logs": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		return envelope, nil, nil
	}

	receipt, err := runSupportAttachment(context.Background(), request, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Uploaded || receipt.Project != "sessions" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if stagedScriptPath == "" {
		t.Fatal("runner did not inspect a staging script")
	}
	if _, err := os.Stat(stagedScriptPath); !os.IsNotExist(err) {
		t.Fatalf("staging script still exists after attachment: %v", err)
	}
}
