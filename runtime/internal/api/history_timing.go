package api

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// Where a slow listing spent its time.
//
// Measured on the owner's MacBook, 11 September, against a daemon that had just
// started: the first GET /api/history took 12.1 s with 265 cards already
// persisted, the second took 0.3 s. A harness timed HistoryStore.List alone at
// 0.38 s cold on the same directories, so twelve seconds were being spent
// somewhere outside the store and nothing in the daemon could say where.
// Production daemons run without pprof, so the answer has to be something the
// daemon itself reports.
//
// This is always on. It records durations only — never a path, a title, or a
// session id — so a slow listing can be diagnosed from a log the owner can
// paste without redacting it first.

// slowListingThreshold is when a listing is worth a line in the log. A listing
// under two seconds is a listing; over it, something is wrong and the operator
// needs the breakdown without being asked to reproduce it under a profiler.
const slowListingThreshold = 2 * time.Second

type stageTimer struct {
	started time.Time
	last    time.Time
	stages  []stage
	// notes are the facts about a listing that are not durations: which of its
	// stages was served from a cache, so a slow one can be told from a stale
	// one without guessing from the numbers.
	notes map[string]any
	// details are durations inside a stage, reported but never added to the
	// total: "the ledger stage took 654 ms and 640 of them were the high-water
	// query" is one stage and two facts, not two stages.
	details map[string]time.Duration
}

type stage struct {
	name string
	took time.Duration
}

func newStageTimer() *stageTimer {
	now := time.Now()
	return &stageTimer{started: now, last: now}
}

// mark closes the stage that has been running since the last mark. Stages that
// repeat under the same name add up, which is what a listing that calls the
// same expensive thing three times should look like.
func (t *stageTimer) mark(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	took := now.Sub(t.last)
	t.last = now
	for index := range t.stages {
		if t.stages[index].name == name {
			t.stages[index].took += took
			return
		}
	}
	t.stages = append(t.stages, stage{name: name, took: took})
}

// markFor records a stage another layer measured. The time it names is part of
// the stage still running here, so it is not double counted: it is subtracted
// from what mark() will attribute next.
func (t *stageTimer) markFor(name string, took time.Duration) {
	if t == nil || took <= 0 {
		return
	}
	t.last = t.last.Add(took)
	for index := range t.stages {
		if t.stages[index].name == name {
			t.stages[index].took += took
			return
		}
	}
	t.stages = append(t.stages, stage{name: name, took: took})
}

// detail records what a stage was made of. It is pure reporting: unlike mark
// and markFor it does not move the cursor, because the time it names is already
// counted inside the stage it explains.
func (t *stageTimer) detail(name string, took time.Duration) {
	if t == nil || took <= 0 {
		return
	}
	if t.details == nil {
		t.details = make(map[string]time.Duration, 4)
	}
	t.details[name] += took
}

func (t *stageTimer) total() time.Duration {
	if t == nil {
		return 0
	}
	return time.Since(t.started)
}

// note records a fact about this listing that is not a duration.
func (t *stageTimer) note(name string, value any) {
	if t == nil {
		return
	}
	if t.notes == nil {
		t.notes = make(map[string]any, 2)
	}
	t.notes[name] = value
}

// breakdown is the timing document a caller asking for it receives: stage name
// to milliseconds, plus the total and any notes. Milliseconds because that is
// the unit the question is asked in, and integers because a reader comparing
// stages does not need nanoseconds.
func (t *stageTimer) breakdown() map[string]any {
	if t == nil {
		return nil
	}
	result := make(map[string]any, len(t.stages)+len(t.notes)+1)
	for _, entry := range t.stages {
		result[entry.name+"_ms"] = entry.took.Milliseconds()
	}
	for name, took := range t.details {
		result[name+"_ms"] = took.Milliseconds()
	}
	for name, value := range t.notes {
		result[name] = value
	}
	result["total_ms"] = t.total().Milliseconds()
	return result
}

// logIfSlow writes the one line the owner asked for, in the order the stages
// ran, largest first within the line's tail so the dominant cost reads first.
func (t *stageTimer) logIfSlow(what string) {
	if t == nil {
		return
	}
	total := t.total()
	if total < slowListingThreshold {
		return
	}
	ordered := append([]stage(nil), t.stages...)
	suffix := ""
	if cached, ok := t.notes["ledger_cached"].(bool); ok && cached {
		suffix = " (ledger cached)"
	}
	sort.SliceStable(ordered, func(left, right int) bool { return ordered[left].took > ordered[right].took })
	parts := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		parts = append(parts, fmt.Sprintf("%s %s", entry.name, round(entry.took)))
	}
	log.Printf("[history] %s took %s: %s%s%s", what, round(total), strings.Join(parts, ", "), t.detailLine(), suffix)
}

// detailLine explains the stages that have an explanation, so the log line a
// person pastes carries the same answer the timing document does.
func (t *stageTimer) detailLine() string {
	if len(t.details) == 0 {
		return ""
	}
	names := make([]string, 0, len(t.details))
	for name := range t.details {
		names = append(names, name)
	}
	sort.SliceStable(names, func(left, right int) bool { return t.details[names[left]] > t.details[names[right]] })
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %s", name, round(t.details[name])))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func round(value time.Duration) string {
	if value >= time.Second {
		return fmt.Sprintf("%.1fs", value.Seconds())
	}
	return fmt.Sprintf("%dms", value.Milliseconds())
}
