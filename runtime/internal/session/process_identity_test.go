package session

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// runnerCommandMatches decides whether an unreachable runner's PID still
// belongs to that runner. A false answer reaps the session's artifacts, so the
// cases below are the difference between keeping and losing live work.
func TestRunnerCommandMatchesKeepsLiveRunners(t *testing.T) {
	const id = "0d1b7f2a-6c33-4a51-9f4e-2b8c7d5e1a90"
	// A plain terminal session carries no explicit kind.
	const terminalKind = ""

	for _, testCase := range []struct {
		name     string
		command  string
		expected string
		kind     string
		want     bool
	}{
		{
			name:    "unknown command is never treated as death",
			command: "",
			kind:    terminalKind,
			want:    true,
		},
		{
			name:    "unix probe reports the runner argv carrying the session id",
			command: "/opt/sessions/sessions-runner --id " + id,
			kind:    terminalKind,
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
			kind:     terminalKind,
			want:     true,
		},
		{
			name:     "structured session hosted directly by the runner",
			command:  `C:\Sessions\sessions-runner.exe`,
			expected: "claude",
			kind:     state.KindClaudeStructured,
			want:     true,
		},
		{
			name:     "an unrelated process at the recorded pid is reuse",
			command:  `C:\Windows\System32\notepad.exe`,
			expected: "powershell.exe",
			kind:     terminalKind,
			want:     false,
		},
		{
			name:     "provider command still matches by base name",
			command:  "/bin/zsh -l",
			expected: "/bin/zsh",
			kind:     terminalKind,
			want:     true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := runnerCommandMatches(testCase.command, id, testCase.expected, testCase.kind)
			if got != testCase.want {
				t.Fatalf("runnerCommandMatches(%q, id, %q, %q) = %v, want %v",
					testCase.command, testCase.expected, testCase.kind, got, testCase.want)
			}
		})
	}
}

// A pid that never belonged to anything is not alive, on either platform.
func TestProcessAliveRejectsNonPIDs(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Fatalf("processAlive(%d) = true, want false", pid)
		}
	}
}
