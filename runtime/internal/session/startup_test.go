package session

import (
	"strings"
	"testing"
	"time"
)

// The startup pass says where its time went, once, when it finishes — the same
// discipline the history listing already follows. On the Mini it is the only
// way to learn which of three minutes was attach and which was everything else.
func TestStartupReportsWhereItsTimeWent(t *testing.T) {
	progress := newStartupProgress()
	progress.setTotal(593)
	for range 593 {
		progress.advance()
	}
	progress.record("scan", 900*time.Millisecond)
	progress.record("attach", 150*time.Second)
	progress.record("attach", 22*time.Second)
	progress.record("ledger", 6*time.Second)

	state := progress.state()
	if state.Phase != StartupLoading || state.Loaded != 593 || state.Total != 593 {
		t.Fatalf("state = %#v, want a loading daemon that has dealt with all 593", state)
	}

	line := progress.finish()
	for _, want := range []string{"593 sessions", "attach 172.0s", "scan 900ms", "ledger 6.0s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("startup line = %q, want it to contain %q", line, want)
		}
	}
	// Repeats of one stage add up: one attach per session, one number.
	if state := progress.state(); state.Phase != StartupReady {
		t.Fatalf("phase after finishing = %q", state.Phase)
	}
	// Only the first pass is startup; a later one must not reopen it or log again.
	progress.advance()
	if line := progress.finish(); line != "" {
		t.Fatalf("a later discovery pass logged a second startup line: %q", line)
	}
	if state := progress.state(); state.Loaded != 593 {
		t.Fatalf("a later pass changed the startup count to %d", state.Loaded)
	}
}
