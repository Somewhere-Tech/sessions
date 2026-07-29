package waitcond

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCommitFiresOnRealCommit(t *testing.T) {
	repo := newGitRepo(t)
	condition, err := NewCommit(context.Background(), "commit-session", repo)
	if err != nil {
		t.Fatal(err)
	}
	commit := condition.(*commitCondition)
	ready := observeWatchRegistration(commit)
	done := startWait(t, commit)
	<-ready
	writeFile(t, filepath.Join(repo, "work.txt"), "second\n")
	git(t, repo, "add", "work.txt")
	git(t, repo, "commit", "-m", "real second commit")
	result, err := waitResult(done)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == result.Baseline || result.Subject != "real second commit" || result.HistoryRewritten {
		t.Fatalf("unexpected commit result: %#v", result)
	}
	t.Logf("commit wait: baseline=%s commit=%s subject=%q history_rewritten=%v", result.Baseline, result.Commit, result.Subject, result.HistoryRewritten)
}

func TestCommitRechecksAfterWatcherRegistration(t *testing.T) {
	repo := newGitRepo(t)
	condition, err := NewCommit(context.Background(), "gap-session", repo)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "work.txt"), "landed before subscribe\n")
	git(t, repo, "add", "work.txt")
	git(t, repo, "commit", "-q", "-m", "event before watcher")

	// The mutation and its fsnotify event happened before Wait registered its
	// watcher. A controlled ticker that never fires proves the immediate
	// post-registration check sees the new HEAD without the poll fallback.
	commit := condition.(*commitCondition)
	intervals := make(chan time.Duration, 1)
	commit.ticker = func(interval time.Duration) conditionTicker {
		intervals <- interval
		return conditionTicker{ticks: make(chan time.Time), stop: func() {}}
	}
	done := startWait(t, commit)
	if interval := <-intervals; interval != commitPollInterval {
		t.Fatalf("poll interval = %s, want %s", interval, commitPollInterval)
	}
	result, err := waitResult(done)
	if err != nil {
		t.Fatal(err)
	}
	if result.Subject != "event before watcher" || result.Commit == result.Baseline {
		t.Fatalf("unexpected gap result: %#v", result)
	}
}

func TestCommitFiveSecondPollFallback(t *testing.T) {
	repo := newGitRepo(t)
	condition, err := NewCommit(context.Background(), "poll-session", repo)
	if err != nil {
		t.Fatal(err)
	}
	commit := condition.(*commitCondition)
	// Force watcher.Add to fail by pointing at a nonexistent parent. The real
	// five-second ticker must remain sufficient for liveness.
	commit.logPath = filepath.Join(t.TempDir(), "missing", "logs", "HEAD")

	checked := make(chan struct{}, 1)
	commit.checked = func() { checked <- struct{}{} }
	ticks := make(chan time.Time)
	intervals := make(chan time.Duration, 1)
	commit.ticker = func(interval time.Duration) conditionTicker {
		intervals <- interval
		return conditionTicker{ticks: ticks, stop: func() {}}
	}
	done := startWait(t, commit)
	if interval := <-intervals; interval != commitPollInterval {
		t.Fatalf("poll interval = %s, want %s", interval, commitPollInterval)
	}
	<-checked
	writeFile(t, filepath.Join(repo, "work.txt"), "poll only\n")
	git(t, repo, "add", "work.txt")
	git(t, repo, "commit", "-q", "-m", "poll fallback")
	select {
	case observed := <-done:
		t.Fatalf("watcher-free wait completed before its poll tick: %#v", observed)
	default:
	}
	ticks <- time.Now()

	observed := <-done
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	if observed.result.Subject != "poll fallback" {
		t.Fatalf("poll fallback result=%#v", observed.result)
	}
	t.Logf("poll-only commit observed on controlled %s tick", commitPollInterval)
}

func TestCommitFlagsForceResetHistoryRewrite(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "work.txt"), "second\n")
	git(t, repo, "add", "work.txt")
	git(t, repo, "commit", "-m", "second")
	condition, err := NewCommit(context.Background(), "rewrite-session", repo)
	if err != nil {
		t.Fatal(err)
	}
	commit := condition.(*commitCondition)
	ready := observeWatchRegistration(commit)
	done := startWait(t, commit)
	<-ready
	git(t, repo, "reset", "--hard", "HEAD^")
	result, err := waitResult(done)
	if err != nil {
		t.Fatal(err)
	}
	if !result.HistoryRewritten || result.Subject != "initial" {
		t.Fatalf("unexpected rewrite result: %#v", result)
	}
	t.Logf("force reset: baseline=%s commit=%s subject=%q history_rewritten=%v", result.Baseline, result.Commit, result.Subject, result.HistoryRewritten)
}

func TestFileContainsAppendAndRecreate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "status.log")
	writeFile(t, path, "starting\n")

	t.Run("append", func(t *testing.T) {
		condition, err := NewFileContains("append-session", root, "status.log", "READY")
		if err != nil {
			t.Fatal(err)
		}
		fileCondition := condition.(*fileContainsCondition)
		ready := observeFileWatchRegistration(fileCondition)
		done := startWait(t, fileCondition)
		<-ready
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("READY\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := waitResult(done)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("append observed: session=%s file=%s literal=%q", result.Session, result.File, result.Contains)
	})

	t.Run("delete-and-recreate", func(t *testing.T) {
		writeFile(t, path, "not yet\n")
		condition, err := NewFileContains("recreate-session", root, path, "DONE")
		if err != nil {
			t.Fatal(err)
		}
		fileCondition := condition.(*fileContainsCondition)
		ready := observeFileWatchRegistration(fileCondition)
		done := startWait(t, fileCondition)
		<-ready
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "replacement DONE\n")
		result, err := waitResult(done)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("recreate observed: session=%s file=%s literal=%q", result.Session, result.File, result.Contains)
	})
}

func TestIdleStableResetsAndReportsEvidenceSource(t *testing.T) {
	root := t.TempDir()
	samples := make(chan bool)
	condition, err := NewIdleStable("idle-session", root, 120*time.Millisecond, func(ctx context.Context) (IdleSample, error) {
		select {
		case working := <-samples:
			return IdleSample{Working: working, Source: "structured"}, nil
		case <-ctx.Done():
			return IdleSample{}, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	idle := condition.(*idleStableCondition)
	clock := newManualClock()
	idle.now = clock.Now
	idle.ticker = clock.Ticker
	done := startWait(t, idle)
	if interval := <-clock.intervals; interval != 30*time.Millisecond {
		t.Fatalf("idle poll interval = %s, want 30ms", interval)
	}
	samples <- false
	clock.Advance(60 * time.Millisecond)
	samples <- true
	clock.Advance(60 * time.Millisecond)
	samples <- false
	clock.Advance(120 * time.Millisecond)
	samples <- false
	result, err := waitResult(done)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != "structured" || result.Elapsed != 240*time.Millisecond {
		t.Fatalf("idle stability did not reset: %#v", result)
	}
}

func TestWaitAnyReturnsFirstSatisfiedCondition(t *testing.T) {
	root := t.TempDir()
	first, err := NewFileContains("first", root, "first.log", "WIN")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileContains("second", root, "second.log", "WIN")
	if err != nil {
		t.Fatal(err)
	}
	firstReady := observeFileWatchRegistration(first.(*fileContainsCondition))
	secondReady := observeFileWatchRegistration(second.(*fileContainsCondition))
	done := startWaitAny(t, []Condition{first, second})
	<-firstReady
	<-secondReady
	writeFile(t, filepath.Join(root, "second.log"), "WIN\n")
	result, err := waitResult(done)
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != "second" {
		t.Fatalf("winner = %q, want second", result.Session)
	}
	t.Logf("--any winner: session=%s condition=%s", result.Session, result.Kind)
}

type waitOutcome struct {
	result Result
	err    error
}

func startWait(t *testing.T, condition Condition) <-chan waitOutcome {
	t.Helper()
	done := make(chan waitOutcome, 1)
	go func() {
		result, err := condition.Wait(t.Context())
		done <- waitOutcome{result: result, err: err}
	}()
	return done
}

func startWaitAny(t *testing.T, conditions []Condition) <-chan waitOutcome {
	t.Helper()
	done := make(chan waitOutcome, 1)
	go func() {
		result, err := WaitAny(t.Context(), conditions)
		done <- waitOutcome{result: result, err: err}
	}()
	return done
}

func waitResult(done <-chan waitOutcome) (Result, error) {
	observed := <-done
	return observed.result, observed.err
}

func observeWatchRegistration(condition *commitCondition) <-chan struct{} {
	ready := make(chan struct{})
	watch := condition.watch
	condition.watch = func(path string) (<-chan struct{}, func()) {
		wake, closeWake := watch(path)
		close(ready)
		return wake, closeWake
	}
	return ready
}

func observeFileWatchRegistration(condition *fileContainsCondition) <-chan struct{} {
	ready := make(chan struct{})
	watch := condition.watch
	condition.watch = func(path string) (<-chan struct{}, func()) {
		wake, closeWake := watch(path)
		close(ready)
		return wake, closeWake
	}
	return ready
}

type manualClock struct {
	mu        sync.Mutex
	now       time.Time
	ticks     chan time.Time
	intervals chan time.Duration
}

func newManualClock() *manualClock {
	return &manualClock{
		now:       time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		ticks:     make(chan time.Time),
		intervals: make(chan time.Duration, 1),
	}
}

func (clock *manualClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *manualClock) Ticker(interval time.Duration) conditionTicker {
	clock.intervals <- interval
	return conditionTicker{ticks: clock.ticks, stop: func() {}}
}

func (clock *manualClock) Advance(elapsed time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(elapsed)
	now := clock.now
	clock.mu.Unlock()
	clock.ticks <- now
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.name", "Sessions Wait Test")
	git(t, repo, "config", "user.email", "sessions-wait@example.invalid")
	writeFile(t, filepath.Join(repo, "work.txt"), "initial\n")
	git(t, repo, "add", "work.txt")
	git(t, repo, "commit", "-q", "-m", "initial")
	return repo
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
