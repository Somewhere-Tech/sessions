package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// finishedChild is the sample a delegated task publishes when the classifier
// has just called it done: idle for the completed reason, not working, and
// silent since.
func finishedChild() completionSample {
	return completionSample{
		IdleReason: state.IdleReasonCompleted,
		Lifecycle:  state.LifecycleTask,
		LastDataAt: 1_000,
		IdleSince:  1_000,
	}
}

func TestTaskCompletionDue(t *testing.T) {
	window := 2 * time.Minute
	scheduled := time.Unix(0, 0)
	past := scheduled.Add(window + time.Second)
	inside := scheduled.Add(window - time.Second)
	idleParent := parentCompletionState{Present: true}

	tests := []struct {
		name     string
		current  func(completionSample) completionSample
		parent   parentCompletionState
		now      time.Time
		expected completionOutcome
	}{
		{
			name:     "sustained quiet with idle parent ends the session",
			now:      past,
			parent:   idleParent,
			expected: completionKill,
		},
		{
			name:     "missing parent does not block cleanup",
			now:      past,
			parent:   parentCompletionState{},
			expected: completionKill,
		},
		{
			name:     "working parent may be about to reuse the child",
			now:      past,
			parent:   parentCompletionState{Present: true, Working: true},
			expected: completionWait,
		},
		{
			name:     "quiet has not lasted the window yet",
			now:      inside,
			parent:   idleParent,
			expected: completionWait,
		},
		{
			name:     "still working child is not finished",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.Working = true; return s },
			expected: completionCancel,
		},
		{
			name:     "terminal output during the window cancels the attempt",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.OutputSeq += 1; return s },
			expected: completionCancel,
		},
		{
			name:   "same-millisecond output is still caught by the sequence",
			now:    past,
			parent: idleParent,
			current: func(s completionSample) completionSample {
				// LastDataAt is unchanged because the write landed inside the
				// same millisecond as the classification.
				s.OutputSeq += 1
				return s
			},
			expected: completionCancel,
		},
		{
			name:     "provider activity that never reaches the terminal cancels the attempt",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.LastDataAt += 1; return s },
			expected: completionCancel,
		},
		{
			name:     "provider activity during the window cancels the attempt",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.ProviderEvents += 1; return s },
			expected: completionCancel,
		},
		{
			name:     "a fresh idle classification supersedes this attempt",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.IdleSince += 1; return s },
			expected: completionCancel,
		},
		{
			name:     "an idle reason other than completed is not a completion",
			now:      past,
			parent:   idleParent,
			current: func(s completionSample) completionSample {
				s.IdleReason = state.IdleReasonNeedsInput
				return s
			},
			expected: completionCancel,
		},
		{
			name:     "a session that stopped being a delegated task is left alone",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.Lifecycle = state.LifecycleSession; return s },
			expected: completionCancel,
		},
		{
			name:     "an exited session has nothing left to end",
			now:      past,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.Exited = true; return s },
			expected: completionCancel,
		},
		{
			name:     "output during the window is refused even before the window elapses",
			now:      inside,
			parent:   idleParent,
			current:  func(s completionSample) completionSample { s.OutputSeq += 1; return s },
			expected: completionCancel,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline := finishedChild()
			current := baseline
			if test.current != nil {
				current = test.current(current)
			}
			got := taskCompletionDue(baseline, current, test.parent, scheduled, test.now, window)
			if got != test.expected {
				t.Fatalf("taskCompletionDue(%+v, parent %+v, now %s) = %v, want %v",
					current, test.parent, test.now.Sub(scheduled), got, test.expected)
			}
		})
	}
}

// completionFixture is a manager with a live parent session and one delegated
// task child, wired to the fake launcher so no PTY is involved.
type completionFixture struct {
	manager  *Manager
	launcher *prototest.Launcher
	store    *ledger.Store
	parent   state.SessionInfo
	child    state.SessionInfo
}

func newCompletionFixture(t *testing.T, options ManagerOptions) *completionFixture {
	t.Helper()
	root := t.TempDir()
	config := testConfig(root)
	config.SettingsPath = filepath.Join(root, "settings.json")
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	options.DisableWatchers = true
	options.ActivityInterval = time.Hour
	options.Boundaries = store.Boundaries()
	options.Observations = store.Observations()
	options.LedgerReader = store
	manager := NewManager(config, launcher, options)
	t.Cleanup(manager.Close)

	parent, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, CreatorSessionID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Lifecycle != state.LifecycleTask {
		t.Fatalf("delegated child lifecycle = %q, want %q", child.Lifecycle, state.LifecycleTask)
	}
	return &completionFixture{manager: manager, launcher: launcher, store: store, parent: parent, child: child}
}

func (f *completionFixture) session(t *testing.T, id string) *state.Session {
	t.Helper()
	session, ok := f.manager.registry.Get(id)
	if !ok {
		t.Fatalf("session %s was not registered", id)
	}
	return session
}

// classifyDone reproduces what publishIdle does when the classifier decides a
// delegated task is finished, without needing a real classification.
func (f *completionFixture) classifyDone(t *testing.T, summary string) {
	t.Helper()
	f.session(t, f.child.ID).SetIdleResult(state.IdleReasonCompleted, "", summary, time.Now().UnixMilli())
	f.manager.scheduleTaskCompletion(f.child.ID)
}

func (f *completionFixture) generation() uint64 {
	f.manager.completionMu.Lock()
	defer f.manager.completionMu.Unlock()
	return f.manager.completionGeneration[f.child.ID]
}

func (f *completionFixture) exited(t *testing.T, id string) bool {
	t.Helper()
	session, ok := f.manager.registry.Get(id)
	return !ok || session.Info().Exited
}

// requireAliveFor asserts the session stays alive for the whole span. The span
// is chosen far wider than the one-second settle this fix replaced, so the old
// behaviour fails it.
func (f *completionFixture) requireAliveFor(t *testing.T, id string, span time.Duration) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(span)
	for time.Now().Before(deadline) {
		if f.exited(t, id) {
			t.Fatalf("delegated task %s was ended after %s; it had to stay alive for %s",
				id, time.Since(started).Round(time.Millisecond), span)
		}
		time.Sleep(waitConditionPoll)
	}
}

// TestTaskCompletionSurvivesTheOldOneSecondSettle pins the sustained-window
// requirement itself: with the production window, a delegated task that the
// classifier called done stays alive well past the one second that used to be
// enough to kill it.
func TestTaskCompletionSurvivesTheOldOneSecondSettle(t *testing.T) {
	fixture := newCompletionFixture(t, ManagerOptions{TaskCompletionPoll: 10 * time.Millisecond})
	fixture.classifyDone(t, "Committed the lane.")
	fixture.requireAliveFor(t, fixture.child.ID, 2500*time.Millisecond)
}

// TestTaskCompletionEndsSustainedQuietTaskWithIdleParent is the other half:
// cleanup still flows once the quiet is genuinely sustained and the parent is
// not working, and it is still attributed to the parent.
func TestTaskCompletionEndsSustainedQuietTaskWithIdleParent(t *testing.T) {
	fixture := newCompletionFixture(t, ManagerOptions{
		TaskCompletionWindow: 200 * time.Millisecond,
		TaskCompletionPoll:   10 * time.Millisecond,
	})
	fixture.classifyDone(t, "Focused work finished.")
	awaitCondition(t, func() bool { return fixture.exited(t, fixture.child.ID) })

	events, err := fixture.store.Events(context.Background(), fixture.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	folded := ledger.Fold(events)[0]
	if folded.EndInitiatorID != fixture.parent.ID ||
		folded.EndClient != "sessionsd-task-lifecycle" ||
		folded.EndReason != "Delegated task completed" {
		t.Fatalf("automatic task end = %#v", folded)
	}
}

// TestTaskCompletionWaitsWhileParentIsWorking pins the parent gate. A working
// orchestrator is exactly the parent that may be about to read or reuse the
// child, so the kill waits; when the orchestrator stops, cleanup resumes
// without needing a new classification.
func TestTaskCompletionWaitsWhileParentIsWorking(t *testing.T) {
	fixture := newCompletionFixture(t, ManagerOptions{
		TaskCompletionWindow: 100 * time.Millisecond,
		TaskCompletionPoll:   10 * time.Millisecond,
	})
	parent := fixture.session(t, fixture.parent.ID)
	parent.SetWorking(true)
	fixture.classifyDone(t, "Lane committed; orchestrator still running.")
	// 25x the window, and 2.5x the settle the old code used.
	fixture.requireAliveFor(t, fixture.child.ID, 2500*time.Millisecond)

	parent.SetWorking(false)
	awaitCondition(t, func() bool { return fixture.exited(t, fixture.child.ID) })
}

// TestTaskCompletionCancelsOnChildOutputAndRescheduleWorks pins that new
// terminal output abandons the attempt outright, and that abandoning is not
// permanent: the next done classification schedules a fresh attempt that can
// still complete.
func TestTaskCompletionCancelsOnChildOutputAndRescheduleWorks(t *testing.T) {
	fixture := newCompletionFixture(t, ManagerOptions{
		TaskCompletionWindow: 100 * time.Millisecond,
		TaskCompletionPoll:   10 * time.Millisecond,
	})
	child := fixture.session(t, fixture.child.ID)
	runner := fixture.launcher.Runner(fixture.child.ID)
	if runner == nil {
		t.Fatal("fake runner for the delegated task was not created")
	}
	fixture.classifyDone(t, "Looked done.")
	before := child.OutputSeq()
	runner.AddOutput("still thinking...\r\n")
	awaitCondition(t, func() bool { return child.OutputSeq() != before })

	// The attempt is cancelled, so the session outlives many windows.
	fixture.requireAliveFor(t, fixture.child.ID, 2500*time.Millisecond)

	// A later classification is not blocked by the cancelled attempt.
	fixture.classifyDone(t, "Actually done now.")
	awaitCondition(t, func() bool { return fixture.exited(t, fixture.child.ID) })
}

// TestTaskCompletionRescheduleSupersedesInFlightAttempt pins that two
// classifications do not race: the older attempt retires rather than killing
// against stale evidence.
func TestTaskCompletionRescheduleSupersedesInFlightAttempt(t *testing.T) {
	fixture := newCompletionFixture(t, ManagerOptions{
		TaskCompletionWindow: 150 * time.Millisecond,
		TaskCompletionPoll:   10 * time.Millisecond,
	})
	fixture.classifyDone(t, "First result.")
	first := fixture.generation()
	fixture.classifyDone(t, "Second result.")
	if second := fixture.generation(); second <= first {
		t.Fatalf("reschedule generation = %d, want greater than %d", second, first)
	}
	awaitCondition(t, func() bool { return fixture.exited(t, fixture.child.ID) })
}
