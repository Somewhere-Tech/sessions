package liveness

import (
	"context"
	"os"
	"testing"
)

// CommandMatches decides whether an unreachable runner's PID still belongs to
// that runner. A false answer reaps the session's artifacts and records a
// permanent loss, so these cases are the difference between keeping and losing
// live work. The table merges what internal/session and internal/recovery each
// asserted about their own copies of this rule.
func TestCommandMatches(t *testing.T) {
	const id = "0d1b7f2a-6c33-4a51-9f4e-2b8c7d5e1a90"

	for _, testCase := range []struct {
		name     string
		command  string
		expected string
		want     bool
	}{
		{
			name:    "unknown command is never treated as death",
			command: "",
			want:    true,
		},
		{
			name:    "unix probe reports the runner argv carrying the session id",
			command: "/opt/sessions/sessions-runner --id " + id,
			want:    true,
		},
		{
			// The Windows probe can only report the image path: the session id
			// lives in a command line this code does not read. Before the
			// platform split this case failed the match and every Windows
			// terminal session was reaped as PID reuse.
			name:     "windows image path alone identifies a runner",
			command:  `C:\Users\a\AppData\Local\Sessions\runtime\v1\sessions-runner.exe`,
			expected: "powershell.exe",
			want:     true,
		},
		{
			name:     "structured session hosted directly by the runner",
			command:  `C:\Sessions\sessions-runner.exe`,
			expected: "claude",
			want:     true,
		},
		{
			name:     "hosted provider command carrying the session id",
			command:  "/opt/homebrew/bin/claude --resume " + id,
			expected: "claude",
			want:     true,
		},
		{
			name:     "legacy typescript runner",
			command:  "node /opt/sessions/runner.js",
			expected: "claude",
			want:     true,
		},
		{
			name:     "an unrelated process at the recorded pid is reuse",
			command:  `C:\Windows\System32\notepad.exe`,
			expected: "powershell.exe",
			want:     false,
		},
		{
			name:     "an unrelated unix process at the recorded pid is reuse",
			command:  "/Applications/Xcode.app/Contents/MacOS/Xcode",
			expected: "claude",
			want:     false,
		},
		{
			name:     "provider command still matches by base name",
			command:  "/bin/zsh -l",
			expected: "/bin/zsh",
			want:     true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := CommandMatches(testCase.command, id, testCase.expected)
			if got != testCase.want {
				t.Fatalf("CommandMatches(%q, id, %q) = %v, want %v",
					testCase.command, testCase.expected, got, testCase.want)
			}
		})
	}
}

func TestProcessAliveAnswersForThisProcess(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("the running test process was reported dead")
	}
	// A pid that never named anything is not alive, on either platform.
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Fatalf("ProcessAlive(%d) = true, want false", pid)
		}
	}
}

// RunnerAlive is the whole question: this session's runner, not "something at
// that number". A recycled PID running an unrelated program is not the runner,
// however alive that program is.
func TestRunnerAliveRejectsRecycledPID(t *testing.T) {
	ctx := context.Background()
	const id = "9a1c0f4d-2b77-4c31-8ee0-5a4d3c2b1099"

	if RunnerAlive(ctx, Runner{SessionID: id, PID: 0, Command: "claude"}) {
		t.Fatal("a runner with no recorded pid was reported alive")
	}
	self := Runner{SessionID: id, PID: os.Getpid(), Command: os.Args[0]}
	if !RunnerAlive(ctx, self) {
		t.Fatal("the running test process did not match its own recorded command")
	}
	// The same live PID, recorded by a session whose runner was something
	// else entirely. The process exists; the runner does not.
	recycled := Runner{
		SessionID: id,
		PID:       os.Getpid(),
		Command:   "/Applications/Xcode.app/Contents/MacOS/Xcode",
	}
	if RunnerAlive(ctx, recycled) {
		t.Fatal("a live pid running an unrelated program was reported as this session's runner")
	}
}
