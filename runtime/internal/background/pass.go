// Package background names the work the daemon does when nobody asked.
//
// From the Mini, 11 September: the startup pass finished in 11.5 s, health said
// ready — and then sessionsd sat at about 100% CPU for minutes, with 900 MB
// resident and nothing in the log naming the work. A daemon that cannot say
// what it is doing to somebody's machine is asking them to guess, and guessing
// is what a profiler is for; production daemons do not run one.
//
// So every pass the daemon starts on its own initiative is started and finished
// here. A pass still running after announceAfter says so, because the case that
// matters is the one that has not ended yet. A pass that took longer than
// logFloor says what it cost when it ends. Shorter passes are counted rather
// than logged — a sweep every 250 ms is real work, and a line every 250 ms is
// not a log — and the totals are on /api/health/deep, where a burst can be read
// while it is happening.
package background

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	// announceAfter is when a pass that has not finished is worth saying out
	// loud. Five seconds is longer than any pass is meant to take and shorter
	// than a person's patience with an unexplained fan.
	announceAfter = 5 * time.Second
	// logFloor is the shortest pass worth its own line. Everything under it is
	// counted instead.
	logFloor = time.Second
)

// Total is what one named pass has cost this process so far.
type Total struct {
	Runs int64         `json:"runs"`
	Wall time.Duration `json:"-"`
	CPU  time.Duration `json:"-"`
}

var (
	mu     sync.Mutex
	totals = map[string]Total{}
)

// Pass is one run of one named piece of background work. A nil Pass is a pass
// that was never started, and every method on it does nothing, so a caller can
// decide not to measure without branching around the call.
type Pass struct {
	name     string
	started  time.Time
	cpu      time.Duration
	hasCPU   bool
	announce *time.Timer
}

func Start(name string) *Pass {
	cpu, ok := processCPU()
	pass := &Pass{name: name, started: time.Now(), cpu: cpu, hasCPU: ok}
	pass.announce = time.AfterFunc(announceAfter, func() {
		log.Printf("[background] %s still running after %s", name, round(announceAfter))
	})
	return pass
}

// Done ends the pass, counts it, and logs it if it was long enough to matter.
//
// The CPU figure is this process's own CPU time over the pass's window, not the
// pass's alone: Go has no per-goroutine CPU clock. On a daemon that is otherwise
// idle — which is the case this exists for — they are the same number, and when
// they are not, the wall time still says how long the pass held the machine's
// attention.
func (p *Pass) Done() {
	if p == nil {
		return
	}
	p.announce.Stop()
	wall := time.Since(p.started)
	var cpu time.Duration
	after, ok := processCPU()
	if ok && p.hasCPU {
		cpu = after - p.cpu
	}
	record(p.name, wall, cpu)
	if wall < logFloor {
		return
	}
	if ok && p.hasCPU {
		log.Printf("[background] %s took %s (process cpu %s)", p.name, round(wall), round(cpu))
		return
	}
	log.Printf("[background] %s took %s", p.name, round(wall))
}

func record(name string, wall, cpu time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	total := totals[name]
	total.Runs++
	total.Wall += wall
	total.CPU += cpu
	totals[name] = total
}

// Totals is every pass this process has run, for a caller that wants to see a
// burst while it is happening rather than after it.
func Totals() map[string]Total {
	mu.Lock()
	defer mu.Unlock()
	copied := make(map[string]Total, len(totals))
	for name, total := range totals {
		copied[name] = total
	}
	return copied
}

// Report is Totals in the shape a JSON document wants: durations in
// milliseconds, because that is the unit the question is asked in.
func Report() map[string]any {
	report := make(map[string]any, len(totals))
	for name, total := range Totals() {
		report[name] = map[string]any{
			"runs": total.Runs, "ms": total.Wall.Milliseconds(), "cpu_ms": total.CPU.Milliseconds(),
		}
	}
	return report
}

func round(value time.Duration) string {
	if value >= time.Second {
		return fmt.Sprintf("%.1fs", value.Seconds())
	}
	return fmt.Sprintf("%dms", value.Milliseconds())
}
