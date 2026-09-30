package session

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// What the daemon is doing before it can answer for every session.
//
// Measured on the installed Mini, 11 September: after a restart with 593
// sessions the daemon spent about three minutes re-attaching runners at 100%
// CPU. During that window a teammate's `sessions wait <lane>` answered "no live
// session matches" — for a lane that existed and was running — because the
// session was not in the registry yet, and nothing anywhere said "not yet".
//
// A daemon that is still loading is not a daemon that has lost your work, and
// the difference has to be something a caller can read. This is that fact:
// which phase the daemon is in, how far through it is, and since when.
type StartupState struct {
	// Phase is "loading" while the first discovery pass is still running and
	// "ready" afterwards. It never goes back: later passes are maintenance,
	// not startup.
	Phase string `json:"phase"`
	// Loaded and Total count the runner records this pass has dealt with —
	// attached, or decided about — against the ones it found. Total is zero
	// until the pass has read the runner directory.
	Loaded int `json:"loaded"`
	Total  int `json:"total"`
	// StartedAt is when this process began loading, in epoch milliseconds.
	StartedAt int64 `json:"startedAt"`
}

const (
	StartupLoading = "loading"
	StartupReady   = "ready"
)

// Loading reports whether a caller asking about a session it cannot see should
// wait rather than conclude the session is gone.
func (s StartupState) Loading() bool { return s.Phase == StartupLoading }

type startupProgress struct {
	mu        sync.Mutex
	phase     string
	loaded    int
	total     int
	startedAt time.Time
	// stages is where the startup pass spent its time, for the one line it
	// logs when it finishes. Same discipline as the history listing: the
	// daemon says where its own time went, because nothing else can.
	stages map[string]time.Duration
	order  []string
}

func newStartupProgress() *startupProgress {
	return &startupProgress{phase: StartupLoading, startedAt: time.Now(), stages: map[string]time.Duration{}}
}

func (p *startupProgress) state() StartupState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return StartupState{
		Phase: p.phase, Loaded: p.loaded, Total: p.total,
		StartedAt: p.startedAt.UnixMilli(),
	}
}

func (p *startupProgress) setTotal(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == StartupLoading {
		p.total = total
	}
}

func (p *startupProgress) advance() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == StartupLoading {
		p.loaded++
	}
}

// record adds to a named stage. Stages repeat — one attach per session — so
// they add up rather than replace.
func (p *startupProgress) record(stage string, took time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase != StartupLoading {
		return
	}
	if _, seen := p.stages[stage]; !seen {
		p.order = append(p.order, stage)
	}
	p.stages[stage] += took
}

// finish ends the loading phase and returns the line to log, or "" when the
// phase had already ended — only the first discovery pass is startup.
func (p *startupProgress) finish() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase != StartupLoading {
		return ""
	}
	p.phase = StartupReady
	total := time.Since(p.startedAt)
	parts := make([]string, 0, len(p.order))
	for _, stage := range p.order {
		parts = append(parts, fmt.Sprintf("%s %s", stage, roundDuration(p.stages[stage])))
	}
	line := fmt.Sprintf("[startup] %d sessions in %s", p.loaded, roundDuration(total))
	if len(parts) > 0 {
		line += " — " + strings.Join(parts, ", ")
	}
	return line
}

func roundDuration(value time.Duration) string {
	if value >= time.Second {
		return fmt.Sprintf("%.1fs", value.Seconds())
	}
	return fmt.Sprintf("%dms", value.Milliseconds())
}

// Startup is what the daemon is doing, for /api/health and anything that has
// to tell a person why an answer is not available yet.
func (m *Manager) Startup() StartupState { return m.startup.state() }

// finishStartup ends the loading phase once, logging where the time went.
func (m *Manager) finishStartup() {
	if line := m.startup.finish(); line != "" {
		log.Print(line)
	}
}
