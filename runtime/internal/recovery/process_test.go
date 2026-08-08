package recovery

import (
	"context"
	"os"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// Recovery's liveness answer now comes from internal/liveness, which owns the
// probe and the PID-reuse rule and tests them directly. What is left to pin
// here is that recovery still asks the shared question, and asks it about the
// runner rather than about a bare PID: recovery's answer decides whether the
// user is offered a second runtime for a conversation that already has one.
func TestProbeProcessAnswersForThisProcess(t *testing.T) {
	ctx := context.Background()
	self := proto.RunnerInfo{ID: "lane", PID: os.Getpid(), Cmd: os.Args[0]}
	if !probeProcess(ctx, self) {
		t.Fatal("the running test process was reported dead")
	}
	for _, pid := range []int{0, -1} {
		if probeProcess(ctx, proto.RunnerInfo{ID: "lane", PID: pid, Cmd: os.Args[0]}) {
			t.Fatalf("probeProcess(pid %d) = true, want false", pid)
		}
	}
	// A live pid recorded by a lane whose runner was something else entirely
	// is PID reuse, not a live runner.
	recycled := proto.RunnerInfo{
		ID:  "44444444-4444-4444-8444-444444444444",
		PID: os.Getpid(),
		Cmd: "/Applications/Xcode.app/Contents/MacOS/Xcode",
	}
	if probeProcess(ctx, recycled) {
		t.Fatal("a reused pid was reported as this lane's runner")
	}
}
