package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCatAndResurrectUseFleetSearchReference(t *testing.T) {
	const historyID = "provider-history:claude:conversation-one"
	var continued string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/history/"+historyID:
			_, _ = io.WriteString(response, "[user]\nRemember the launch.\n")
		case request.Method == http.MethodPost && request.URL.Path == "/api/recovery/adopt":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			continued, _ = body["historyId"].(string)
			_, _ = io.WriteString(response, `{"ok":true,"laneId":"continued-lane","adoption":{}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mini", Endpoint: server.URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"cat", "mini::" + historyID}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 || stdout.String() != "[user]\nRemember the launch.\n" {
		t.Fatalf("cat exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"resurrect", "mini::" + historyID}, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != "continued-lane\n" {
		t.Fatalf("resurrect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if continued != historyID {
		t.Fatalf("continued history = %q", continued)
	}
}
