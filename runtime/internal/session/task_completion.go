package session

import (
	"context"
	"log"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// taskCompletionSustainedWindow is how long a delegated task session must stay
// completed, not working, and silent before the daemon ends it.
//
// The idle classifier reads terminal output and infers "done" from the
// outside. That inference is wrong often enough to matter: it has called a
// healthy session failed for discussing a failure, and a session sitting at
// its composer finished. One second of quiet between an agent's turns is
// ordinary, so a one-second settle turned an ordinary pause into a kill of
// in-flight work. Per docs/PHILOSOPHY.md a classifier's wrong guess may cost a
// nap, never a death; until message-wake sleep exists, the narrowest honest
// substitute is to require the quiet to be sustained far past any normal
// inter-turn pause. Two minutes is longer than an agent pauses between turns
// and short enough that finished delegates do not accumulate.
const taskCompletionSustainedWindow = 2 * time.Minute

// taskCompletionPollInterval is how often a pending completion re-reads the
// child and its parent while the sustained window elapses.
const taskCompletionPollInterval = 5 * time.Second

// completionOutcome is the verdict of one taskCompletionDue evaluation.
type completionOutcome int

const (
	// completionWait means nothing disqualifies the kill yet but the evidence
	// is not complete: keep observing.
	completionWait completionOutcome = iota
	// completionCancel abandons this completion attempt entirely. It is not a
	// permanent decision about the session: the next time the session goes
	// from working to idle with a done classification, publishIdle schedules a
	// fresh attempt.
	completionCancel
	// completionKill means the session has been demonstrably finished and
	// quiet for the whole sustained window and its parent is not working.
	completionKill
)

// completionSample is the evidence a delegated task session offers about
// whether it is still doing anything. Every field is read from the session's
// own published state, so the decision below is a pure function of two
// samples: the one taken when the attempt was scheduled and the current one.
type completionSample struct {
	Exited     bool
	Working    bool
	IdleReason string
	Lifecycle  string
	// OutputSeq is the monotonic sequence of the last terminal output applied.
	// It is the primary "did anything come out of it" signal; LastDataAt
	// carries millisecond granularity and so can miss same-millisecond writes.
	OutputSeq uint32
	// LastDataAt advances on terminal output and on provider activity events.
	LastDataAt int64
	// IdleSince is restamped by every idle publication, so a change means a
	// fresh classification replaced the one this attempt was scheduled for.
	IdleSince int64
	// ProviderEvents counts structured provider events, which is how a
	// structured session shows activity that never reaches the terminal.
	ProviderEvents int64
}

// parentCompletionState is what the decision needs to know about the parent
// that delegated the work. Present is false when the parent session can no
// longer be resolved: a parent that is already gone cannot be about to read
// its child, so its absence must not block cleanup.
type parentCompletionState struct {
	Present bool
	Working bool
}

// taskCompletionDue decides whether a scheduled delegated-task cleanup may
// proceed. It is deliberately pure so the whole policy is testable without a
// PTY, a runner, or a clock.
//
// The kill is refused whenever the session might still be doing something, or
// whenever the parent orchestrator is working — an orchestrator mid-turn is
// exactly the parent that may be about to read, message, or reuse the child.
// A parent that is asleep, idle, or gone keeps cleanup flowing.
func taskCompletionDue(
	baseline, current completionSample,
	parent parentCompletionState,
	sustainedSince, now time.Time,
	window time.Duration,
) completionOutcome {
	switch {
	case current.Exited:
		// Already over; there is nothing left to end.
		return completionCancel
	case current.Working:
		// The classifier's "done" did not survive contact with the session.
		return completionCancel
	case current.IdleReason != state.IdleReasonCompleted:
		return completionCancel
	case current.Lifecycle != state.LifecycleTask:
		return completionCancel
	case current.OutputSeq != baseline.OutputSeq,
		current.LastDataAt != baseline.LastDataAt,
		current.ProviderEvents != baseline.ProviderEvents,
		current.IdleSince != baseline.IdleSince:
		// New output, new provider activity, or a fresh idle publication. The
		// completion this attempt was scheduled for is stale, so the attempt
		// is abandoned rather than fossilized against old evidence.
		return completionCancel
	case now.Sub(sustainedSince) < window:
		return completionWait
	case parent.Present && parent.Working:
		// Not a cancellation: the child is still demonstrably finished. The
		// attempt simply waits for the orchestrator to stop working.
		return completionWait
	}
	return completionKill
}

func sampleCompletion(session *state.Session) completionSample {
	info := session.Info()
	sample := completionSample{
		Exited:         info.Exited,
		Working:        info.Working,
		IdleReason:     info.IdleReason,
		Lifecycle:      info.Lifecycle,
		OutputSeq:      session.OutputSeq(),
		LastDataAt:     info.LastDataAt,
		ProviderEvents: session.ClaudeEventCount(),
	}
	if info.IdleSince != nil {
		sample.IdleSince = *info.IdleSince
	}
	return sample
}

func (m *Manager) parentCompletionState(id string) parentCompletionState {
	session, ok := m.registry.Get(id)
	if !ok {
		return parentCompletionState{}
	}
	info := session.Info()
	if info.Exited {
		// An exited parent is present in the registry's short grace window but
		// is no longer anyone who could reuse the child.
		return parentCompletionState{}
	}
	return parentCompletionState{Present: true, Working: info.Working}
}

// completionAttempt claims the newest completion attempt for a session and
// returns its generation. Scheduling always supersedes any attempt already in
// flight, which is what keeps completion re-schedulable instead of one-shot.
func (m *Manager) completionAttempt(id string) uint64 {
	m.completionMu.Lock()
	defer m.completionMu.Unlock()
	m.completionGeneration[id]++
	return m.completionGeneration[id]
}

func (m *Manager) completionAttemptCurrent(id string, generation uint64) bool {
	m.completionMu.Lock()
	defer m.completionMu.Unlock()
	return m.completionGeneration[id] == generation
}

func (m *Manager) forgetCompletionAttempt(id string, generation uint64) {
	m.completionMu.Lock()
	defer m.completionMu.Unlock()
	if m.completionGeneration[id] == generation {
		delete(m.completionGeneration, id)
	}
}

func (m *Manager) taskCompletionWindow() time.Duration {
	if m.options.TaskCompletionWindow > 0 {
		return m.options.TaskCompletionWindow
	}
	return taskCompletionSustainedWindow
}

func (m *Manager) taskCompletionPoll() time.Duration {
	if m.options.TaskCompletionPoll > 0 {
		return m.options.TaskCompletionPoll
	}
	return taskCompletionPollInterval
}

// scheduleTaskCompletion watches a delegated task session that the idle
// classifier believes is finished, and ends it only once the finish has held
// still for the sustained window with a parent that is not working. Any
// contradicting evidence abandons the attempt; the next done classification
// schedules another one.
func (m *Manager) scheduleTaskCompletion(id string) {
	session, ok := m.registry.Get(id)
	if !ok {
		return
	}
	// The baseline is taken here, in the caller's goroutine, so it is the
	// evidence as of the classification that scheduled this attempt. Sampling
	// it inside the worker instead would let output that arrived before the
	// worker was scheduled be absorbed into the baseline and go unnoticed.
	baseline := sampleCompletion(session)
	sustainedSince := time.Now()
	generation := m.completionAttempt(id)
	m.startWorker(func() {
		defer m.forgetCompletionAttempt(id, generation)
		window := m.taskCompletionWindow()
		poll := m.taskCompletionPoll()
		if poll > window {
			poll = window
		}
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
			}
			if !m.completionAttemptCurrent(id, generation) {
				// A newer classification took over this session.
				return
			}
			current, present := m.registry.Get(id)
			if !present {
				return
			}
			info := current.Info()
			provenance := m.withProvenance(context.Background(), []state.SessionInfo{info})[0]
			if provenance.ParentSessionID == "" {
				// Without a parent there is no initiator to attribute the end
				// to, and nothing that delegated the work in the first place.
				return
			}
			outcome := taskCompletionDue(
				baseline, sampleCompletion(current),
				m.parentCompletionState(provenance.ParentSessionID),
				sustainedSince, time.Now(), window,
			)
			switch outcome {
			case completionWait:
				continue
			case completionCancel:
				return
			}
			if err := m.RequestKillAttributed(context.Background(), id, false, state.EndSessionRequest{
				InitiatorKind: string(ledger.CreatorSession),
				InitiatorID:   provenance.ParentSessionID,
				Client:        "sessionsd-task-lifecycle",
				Reason:        "Delegated task completed",
			}); err != nil {
				// Completion remains visible and resumable if the durable end
				// boundary cannot be recorded. Never trade history for
				// cleanup, and never retry a refused kill in a loop: report
				// the fact once and leave the session alive.
				log.Printf("[task-lifecycle] end completed delegated session %s: %v", id, err)
			}
			return
		}
	})
}
