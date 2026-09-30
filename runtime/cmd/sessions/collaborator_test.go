package main

import (
	"strings"
	"testing"
)

func TestBriefingStdinUsesExplicitCollaboratorRoute(t *testing.T) {
	a := &app{stdin: strings.NewReader("Do not stop the original agent.")}
	body := map[string]any{"sourceSessionId": "source"}
	path, err := a.prepareForkContext(body, "-", true, false, "second-account", true)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/recovery/collaborator" || body["contextMode"] != "briefing" || body["profile"] != "second-account" || body["briefing"] != "Do not stop the original agent." {
		t.Fatalf("path=%s body=%#v", path, body)
	}
	a.stdin = strings.NewReader(strings.Repeat("x", 24*1024+1))
	if _, err := a.prepareForkContext(map[string]any{}, "-", true, false, "", false); err == nil {
		t.Fatal("oversized input accepted")
	}
}
