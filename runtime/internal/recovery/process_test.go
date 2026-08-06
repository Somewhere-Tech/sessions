package recovery

import (
	"context"
	"os"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestProcessAliveAnswersForThisProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("the running test process was reported dead")
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Fatalf("processAlive(%d) = true, want false", pid)
		}
	}
	if probeProcess(context.Background(), proto.RunnerInfo{ID: "lane", PID: 0}) {
		t.Fatal("a runner with no recorded pid was reported running")
	}
}

func TestRunnerProcessMatchesRejectsPIDReuse(t *testing.T) {
	id := "44444444-4444-4444-8444-444444444444"
	for _, test := range []struct {
		name    string
		command string
		want    bool
	}{
		{name: "lane id in the command line", command: "sessions-runner --id " + id, want: true},
		{name: "runner image path only", command: `C:\Program Files\Sessions\sessions-runner.exe`, want: true},
		{name: "hosted provider command", command: "/opt/homebrew/bin/claude --resume " + id, want: true},
		{name: "unrelated process reused the pid", command: "/Applications/Xcode.app/Contents/MacOS/Xcode", want: false},
		// An unreadable command line is not evidence of death. The session
		// manager treats it as live, and recovery must not disagree with the
		// component that owns the runner.
		{name: "unknown command", command: "", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := runnerProcessMatches(test.command, id, "claude"); got != test.want {
				t.Fatalf("runnerProcessMatches(%q) = %t, want %t", test.command, got, test.want)
			}
		})
	}
}
