package resource

import (
	"errors"
	"testing"
	"time"
)

// fakeEnumerator is the seam that makes tree walking testable without a
// machine: a fabricated process table, returned verbatim.
type fakeEnumerator struct {
	tables [][]Process
	err    error
	calls  int
}

func (f *fakeEnumerator) Enumerate() ([]Process, error) {
	if f.err != nil {
		return nil, f.err
	}
	index := f.calls
	if index >= len(f.tables) {
		index = len(f.tables) - 1
	}
	f.calls++
	return f.tables[index], nil
}

func TestCPUPercentFromTwoCumulativeSamples(t *testing.T) {
	// Half a second of CPU burned over one second of wall time is half a core.
	percent, ok := CPUPercent(2*time.Second, 2500*time.Millisecond, time.Second)
	if !ok {
		t.Fatalf("expected a rate from two cumulative samples")
	}
	if percent != 50 {
		t.Fatalf("percent = %v, want 50", percent)
	}
}

func TestCPUPercentIsNotALifetimeAverage(t *testing.T) {
	// The mistake this package exists to avoid. A process that burned an hour
	// of CPU over its life and nothing at all in the last five seconds is idle
	// now. `ps %cpu` would report it as busy; the delta reports the truth.
	percent, ok := CPUPercent(time.Hour, time.Hour, 5*time.Second)
	if !ok {
		t.Fatalf("expected a rate")
	}
	if percent != 0 {
		t.Fatalf("percent = %v, want 0 for a process that burned nothing in the window", percent)
	}
}

func TestCPUPercentAboveOneCore(t *testing.T) {
	// Four seconds of CPU in one second of wall time is four cores. Clamping
	// this to 100 would hide the sessions that matter most.
	percent, ok := CPUPercent(0, 4*time.Second, time.Second)
	if !ok || percent != 400 {
		t.Fatalf("percent = %v ok = %v, want 400 true", percent, ok)
	}
}

func TestCPUPercentZeroElapsedIsUnknown(t *testing.T) {
	if percent, ok := CPUPercent(time.Second, 2*time.Second, 0); ok {
		t.Fatalf("expected unknown with no elapsed wall time, got %v", percent)
	}
	if percent, ok := CPUPercent(time.Second, 2*time.Second, -time.Second); ok {
		t.Fatalf("expected unknown with negative elapsed time, got %v", percent)
	}
}

func TestCPUPercentCounterResetIsUnknown(t *testing.T) {
	// A cumulative counter cannot decrease. When it does, the PID was reused
	// by a different process. Unknown is the only honest answer -- not zero,
	// which would read as an idle session, and not a negative percentage.
	percent, ok := CPUPercent(10*time.Second, 1*time.Second, time.Second)
	if ok {
		t.Fatalf("expected unknown after a counter reset, got %v", percent)
	}
	if percent != 0 {
		t.Fatalf("percent = %v, want the zero value alongside ok=false", percent)
	}
}

func TestFirstSampleHasNoPercentYet(t *testing.T) {
	enumerator := &fakeEnumerator{tables: [][]Process{{
		{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 10 << 20, CPUTime: 3 * time.Second},
	}}}
	clock := time.Unix(1000, 0)
	tracker := NewTracker(enumerator, func() time.Time { return clock })

	samples, err := tracker.Sample(map[string]int{"a": 100})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	first := samples["a"]
	if !first.Known {
		t.Fatalf("expected a known memory sample on the first pass")
	}
	if first.RSSBytes != 10<<20 {
		t.Fatalf("rss = %d, want %d", first.RSSBytes, 10<<20)
	}
	if first.CPUKnown {
		t.Fatalf("first sample must not claim a CPU rate: there is nothing to subtract from")
	}

	// Second pass, five seconds later, one more second of CPU: 20%.
	clock = clock.Add(5 * time.Second)
	enumerator.tables = [][]Process{{
		{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 11 << 20, CPUTime: 4 * time.Second},
	}}
	enumerator.calls = 0
	samples, err = tracker.Sample(map[string]int{"a": 100})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	second := samples["a"]
	if !second.CPUKnown {
		t.Fatalf("second sample should have a rate")
	}
	if second.CPUPercent != 20 {
		t.Fatalf("percent = %v, want 20", second.CPUPercent)
	}
	if !second.At.Equal(clock) {
		t.Fatalf("sampledAt = %v, want %v", second.At, clock)
	}
}

func TestTreeWalkAggregatesDescendants(t *testing.T) {
	// A PTY session as it really is: the daemon is told the provider's PID,
	// the runner is its parent, and the provider has spawned children of its
	// own. All of it is one session's cost.
	table := NewTable([]Process{
		{PID: 1, PPID: 0, Name: "launchd"},
		{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 20 << 20, CPUTime: time.Second},
		{PID: 200, PPID: 100, Name: "claude", RSSBytes: 100 << 20, CPUTime: 10 * time.Second},
		{PID: 300, PPID: 200, Name: "rg", RSSBytes: 5 << 20, CPUTime: 2 * time.Second},
		{PID: 400, PPID: 300, Name: "grandchild", RSSBytes: 1 << 20, CPUTime: 500 * time.Millisecond},
		{PID: 999, PPID: 1, Name: "unrelated", RSSBytes: 900 << 20, CPUTime: time.Hour},
	})

	if root := table.Root(200); root != 100 {
		t.Fatalf("root of a PTY provider = %d, want its runner 100", root)
	}
	tree := table.Tree(table.Root(200))
	if len(tree) != 4 {
		t.Fatalf("tree covered %d processes, want 4 (runner, provider, child, grandchild)", len(tree))
	}
	var rss uint64
	for _, process := range tree {
		rss += process.RSSBytes
		if process.PID == 999 {
			t.Fatalf("an unrelated process leaked into the tree")
		}
	}
	if want := uint64(126 << 20); rss != want {
		t.Fatalf("tree rss = %d, want %d", rss, want)
	}
}

func TestRootStaysPutForAStructuredRunner(t *testing.T) {
	// A structured runner reports its own PID, so there is nothing to climb
	// to. Climbing anyway would attribute launchd to the session.
	table := NewTable([]Process{
		{PID: 1, PPID: 0, Name: "launchd"},
		{PID: 100, PPID: 1, Name: "sessions-runner"},
	})
	if root := table.Root(100); root != 100 {
		t.Fatalf("root = %d, want 100", root)
	}
}

func TestRootDoesNotClimbToAnUnrelatedParent(t *testing.T) {
	// A provider whose parent is a shell, not a runner, is measured where it
	// stands. Climbing on any parent would let one session absorb another's.
	table := NewTable([]Process{
		{PID: 50, PPID: 1, Name: "zsh"},
		{PID: 200, PPID: 50, Name: "claude"},
	})
	if root := table.Root(200); root != 200 {
		t.Fatalf("root = %d, want 200", root)
	}
}

func TestTreeOfAMissingPIDIsEmpty(t *testing.T) {
	table := NewTable([]Process{{PID: 1, PPID: 0, Name: "launchd"}})
	if tree := table.Tree(4242); len(tree) != 0 {
		t.Fatalf("tree of an absent pid returned %d processes", len(tree))
	}
}

func TestSampleReportsUnknownForADeadSession(t *testing.T) {
	// The honesty requirement: a session whose process is gone reports
	// unknown, not zero, and reports it every time thereafter.
	enumerator := &fakeEnumerator{tables: [][]Process{
		{{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 10 << 20, CPUTime: time.Second}},
		{{PID: 1, PPID: 0, Name: "launchd"}},
	}}
	clock := time.Unix(2000, 0)
	tracker := NewTracker(enumerator, func() time.Time { return clock })

	if sample := mustSample(t, tracker, map[string]int{"a": 100})["a"]; !sample.Known {
		t.Fatalf("expected the live pass to be known")
	}
	clock = clock.Add(5 * time.Second)
	sample := mustSample(t, tracker, map[string]int{"a": 100})["a"]
	if sample.Known {
		t.Fatalf("a session whose process is gone must report unknown")
	}
	if sample.RSSBytes != 0 || sample.CPUKnown {
		t.Fatalf("an unknown sample must carry no figures: %+v", sample)
	}
}

func TestSampleReportsUnknownForASessionWithNoPID(t *testing.T) {
	enumerator := &fakeEnumerator{tables: [][]Process{{{PID: 1, PPID: 0, Name: "launchd"}}}}
	tracker := NewTracker(enumerator, func() time.Time { return time.Unix(3000, 0) })
	sample := mustSample(t, tracker, map[string]int{"never-started": 0})["never-started"]
	if sample.Known {
		t.Fatalf("a session with no pid must report unknown")
	}
}

func TestSampleSurvivesAChildExiting(t *testing.T) {
	// Restate's reason for existing. Between the two samples the provider's
	// child exits, taking a large cumulative counter with it. Subtracting tree
	// totals would go negative and report unknown; per-PID restatement keeps
	// the surviving processes' real delta.
	clock := time.Unix(4000, 0)
	enumerator := &fakeEnumerator{tables: [][]Process{
		{
			{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 20 << 20, CPUTime: time.Second},
			{PID: 200, PPID: 100, Name: "claude", RSSBytes: 100 << 20, CPUTime: 10 * time.Second},
			{PID: 300, PPID: 200, Name: "compiler", RSSBytes: 500 << 20, CPUTime: 60 * time.Second},
		},
		{
			{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 20 << 20, CPUTime: time.Second},
			{PID: 200, PPID: 100, Name: "claude", RSSBytes: 100 << 20, CPUTime: 11 * time.Second},
		},
	}}
	tracker := NewTracker(enumerator, func() time.Time { return clock })
	mustSample(t, tracker, map[string]int{"a": 200})
	clock = clock.Add(10 * time.Second)
	sample := mustSample(t, tracker, map[string]int{"a": 200})["a"]
	if !sample.CPUKnown {
		t.Fatalf("a child exiting must not destroy the rate")
	}
	if sample.CPUPercent != 10 {
		t.Fatalf("percent = %v, want 10 (one second of surviving CPU over ten)", sample.CPUPercent)
	}
	if sample.Processes != 2 {
		t.Fatalf("processes = %d, want 2", sample.Processes)
	}
}

func TestRestateProjectsPreviousOntoCurrentPIDs(t *testing.T) {
	previous := map[int]time.Duration{100: time.Second, 300: time.Minute}
	current := map[int]time.Duration{100: 2 * time.Second, 400: 5 * time.Second}
	previousTotal, currentTotal := Restate(previous, current)
	// 300 is gone and must not be subtracted; 400 is new and its whole
	// counter belongs to the window.
	if previousTotal != time.Second {
		t.Fatalf("previousTotal = %v, want 1s", previousTotal)
	}
	if currentTotal != 7*time.Second {
		t.Fatalf("currentTotal = %v, want 7s", currentTotal)
	}
}

func TestSampleForgetsSessionsNotAskedAbout(t *testing.T) {
	clock := time.Unix(5000, 0)
	enumerator := &fakeEnumerator{tables: [][]Process{{
		{PID: 100, PPID: 1, Name: "sessions-runner", RSSBytes: 1 << 20, CPUTime: time.Second},
	}}}
	tracker := NewTracker(enumerator, func() time.Time { return clock })
	mustSample(t, tracker, map[string]int{"a": 100})
	clock = clock.Add(5 * time.Second)
	mustSample(t, tracker, map[string]int{})
	clock = clock.Add(5 * time.Second)
	// "a" is asked about again after being dropped: it is a first sample once
	// more, so no rate is claimed from a stale reading ten seconds old.
	sample := mustSample(t, tracker, map[string]int{"a": 100})["a"]
	if sample.CPUKnown {
		t.Fatalf("a forgotten session must not produce a rate from a stale reading")
	}
}

func TestSampleReturnsEnumerationErrors(t *testing.T) {
	failure := errors.New("no process table")
	tracker := NewTracker(&fakeEnumerator{err: failure}, nil)
	if _, err := tracker.Sample(map[string]int{"a": 1}); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want %v", err, failure)
	}
}

func mustSample(t *testing.T, tracker *Tracker, roots map[string]int) map[string]Sample {
	t.Helper()
	samples, err := tracker.Sample(roots)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	return samples
}
