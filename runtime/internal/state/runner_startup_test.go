package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// From the owner's recoveries on 11 September: a runner that failed to start
// surfaced as `runner did not create socket within 60s` — sixty seconds later,
// with no cause. The runner had written the cause to its own log, which nothing
// was reading.
func TestAFailedStartupSaysWhatTheRunnerPrinted(t *testing.T) {
	for name, test := range map[string]struct {
		log  string
		want string
	}{
		"provider trust prompt": {
			log:  "Claude Code\nDo you trust the files in this folder?\n1. Yes, I trust this folder\n",
			want: "trust this folder",
		},
		"missing working directory": {
			log:  "runner: chdir /Users/example/gone: no such file or directory\n",
			want: "working directory",
		},
		"not executable": {
			log:  "exec /usr/local/bin/claude: permission denied\n",
			want: "permission denied",
		},
		"anything else, in the runner's own words": {
			log:  "node: internal error: heap out of memory\n",
			want: "heap out of memory",
		},
	} {
		directory := t.TempDir()
		id := "22222222-3333-4444-5555-666666666666"
		if err := os.WriteFile(filepath.Join(directory, id+".log"), []byte(test.log), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := startupFailure(directory, id); !strings.Contains(got, test.want) {
			t.Errorf("%s: cause = %q, want it to mention %q", name, got, test.want)
		}
	}
}

func TestASilentLogLeavesTheSocketTimeoutAlone(t *testing.T) {
	options := waitOptions{RunnerStateDir: t.TempDir(), ID: "quiet", Deadline: time.Second}
	err := options.failure("/tmp/quiet.sock", "", os.ErrNotExist)
	if !strings.Contains(err.Error(), "did not create socket") {
		t.Fatalf("error = %v, want the socket timeout when the runner said nothing", err)
	}
}

// A launcher that owns the process knows the moment it is not coming. Waiting
// out the deadline for a process that exited two seconds ago taught the owner
// nothing except that Sessions was not watching.
func TestTheWaitEndsWhenTheRunnerDoes(t *testing.T) {
	directory := t.TempDir()
	stopped := false
	started := time.Now()
	_, err := waitForRunner(context.Background(), func() (proto.Runner, error) {
		return nil, os.ErrNotExist
	}, "/tmp/gone.sock", waitOptions{
		RunnerStateDir: directory, ID: "gone", Deadline: 30 * time.Second,
		Stopped: func() (string, bool) {
			if time.Since(started) > 50*time.Millisecond {
				stopped = true
			}
			return "exited (exit status 1)", stopped
		},
	})
	if err == nil {
		t.Fatal("the wait returned a runner that never existed")
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the wait took %s after the process was known to be gone", took)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v, want the runner's exit status", err)
	}
}
