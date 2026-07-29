package main

import (
	"errors"
	"strings"
	"testing"
)

func TestStructuredProfileLoginHintKeepsToolErrorAndTeachesPTYLogin(t *testing.T) {
	toolErr := errors.New("Claude Code is not logged in")
	if got := structuredProfileLoginHint(toolErr, "work"); !errors.Is(got, toolErr) ||
		!strings.Contains(got.Error(), "Claude Code is not logged in") ||
		!strings.Contains(got.Error(), "new profile: open a regular PTY session with --profile work once to log in") {
		t.Fatalf("profile hint = %v", got)
	}
	if got := structuredProfileLoginHint(toolErr, ""); got != toolErr {
		t.Fatalf("default structured error changed: %v", got)
	}
}
