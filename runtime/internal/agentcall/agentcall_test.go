package agentcall

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCodexArgumentsDisableToolBearingFeatures(t *testing.T) {
	arguments := Arguments(ProviderCodex)
	joined := strings.Join(arguments, " ")
	for _, feature := range []string{
		"shell_tool", "unified_exec", "code_mode_host", "apps", "plugins",
		"browser_use", "browser_use_external", "browser_use_full_cdp_access", "in_app_browser",
		"computer_use", "image_generation", "multi_agent", "goals", "hooks", "remote_plugin",
		"workspace_dependencies", "skill_mcp_dependency_install", "tool_suggest", "auth_elicitation",
		"tool_call_mcp_elicitation",
	} {
		if !hasPair(arguments, "--disable", feature) {
			t.Errorf("Codex arguments do not disable %s: %s", feature, joined)
		}
	}
	if !hasPair(arguments, "-c", `web_search="disabled"`) {
		t.Errorf("Codex arguments do not disable web search: %s", joined)
	}
	for _, required := range []string{"--ephemeral", "--ignore-user-config", "--ignore-rules", "read-only"} {
		if !slices.Contains(arguments, required) {
			t.Errorf("Codex arguments missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "--model") {
		t.Fatalf("Codex arguments hardcode a model: %s", joined)
	}
}

func TestClaudeArgumentsDisableToolsAndPersistence(t *testing.T) {
	arguments := Arguments(ProviderClaude)
	if !hasPair(arguments, "--tools", "") || !slices.Contains(arguments, "--strict-mcp-config") ||
		!slices.Contains(arguments, "--safe-mode") || !slices.Contains(arguments, "--no-chrome") ||
		!slices.Contains(arguments, "--disable-slash-commands") || !hasPair(arguments, "--setting-sources", "") ||
		!slices.Contains(arguments, "--no-session-persistence") {
		t.Fatalf("Claude arguments are not isolated: %#v", arguments)
	}
}

func TestMissingCodexFeatures(t *testing.T) {
	var output strings.Builder
	for _, feature := range requiredCodexFeatures {
		if feature != "hooks" {
			output.WriteString(feature + " stable true\n")
		}
	}
	missing := missingCodexFeatures(output.String())
	if len(missing) != 1 || missing[0] != "hooks" {
		t.Fatalf("missing=%#v, want hooks", missing)
	}
}

// Both provider CLIs fork helper processes that inherit the output pipe, so a
// cancelled call used to block in Wait forever. These tests stand in for that
// shape with a script that backgrounds a long sleep holding the same pipe.
func fakeProviderCLI(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake provider CLI is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shortWaitDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	previous := waitDelay
	waitDelay = delay
	t.Cleanup(func() { waitDelay = previous })
}

func TestRunIsolatedReturnsWhenAHelperHoldsTheOutputPipe(t *testing.T) {
	shortWaitDelay(t, 200*time.Millisecond)
	executable := fakeProviderCLI(t, "sleep 60 &\nsleep 60\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runIsolated(ctx, ProviderClaude, "smart search", executable, nil, t.TempDir(), "prompt")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runIsolated succeeded, want a timeout error")
		}
		if !strings.Contains(err.Error(), "smart search timed out") {
			t.Fatalf("err = %v, want an instructional smart search timeout", err)
		}
		if !strings.Contains(err.Error(), "installed and signed in") {
			t.Fatalf("err = %v, want the safe next action", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runIsolated blocked past the deadline on the inherited output pipe")
	}
}

func TestRunIsolatedKeepsOutputWhenOnlyAHelperOutlivesACleanExit(t *testing.T) {
	shortWaitDelay(t, 200*time.Millisecond)
	executable := fakeProviderCLI(t, "sleep 60 &\nprintf '{\"query\":\"ok\"}'\n")

	done := make(chan struct{})
	var output string
	var err error
	go func() {
		defer close(done)
		output, err = runIsolated(context.Background(), ProviderClaude, "smart search", executable, nil, t.TempDir(), "prompt")
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runIsolated blocked after the CLI exited cleanly")
	}
	if err != nil {
		t.Fatalf("runIsolated = %v, want the captured answer", err)
	}
	if output != `{"query":"ok"}` {
		t.Fatalf("output = %q, want the complete captured answer", output)
	}
}

func TestRunIsolatedReportsCLIFailureDetail(t *testing.T) {
	executable := fakeProviderCLI(t, "echo 'not signed in' >&2\nexit 1\n")
	_, err := runIsolated(context.Background(), ProviderCodex, "daily recap", executable, nil, t.TempDir(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "codex daily recap call failed: not signed in") {
		t.Fatalf("err = %v, want the CLI's own stderr detail", err)
	}
}

func TestBoundedContextDefendsAgainstAnUnboundedCaller(t *testing.T) {
	ctx, cancel := boundedContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("boundedContext left an unbounded call unbounded")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > defaultCallTimeout {
		t.Fatalf("remaining = %s, want at most %s", remaining, defaultCallTimeout)
	}
}

func TestBoundedContextKeepsACallerDeadline(t *testing.T) {
	caller, cancelCaller := context.WithTimeout(context.Background(), time.Minute)
	defer cancelCaller()
	ctx, cancel := boundedContext(caller)
	defer cancel()
	callerDeadline, _ := caller.Deadline()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(callerDeadline) {
		t.Fatalf("deadline = %v (%t), want the caller's %v", deadline, ok, callerDeadline)
	}
}

func hasPair(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}
