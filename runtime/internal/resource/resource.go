// Package resource measures what a session costs the machine it runs on.
//
// Sessions has always been able to say how many tokens a conversation spent
// and nothing at all about the memory and CPU its processes hold. That gap is
// not cosmetic: a machine can be carrying two hundred agent processes, most of
// them idle, and every listing Sessions prints will show them as equally
// weightless. A session manager that cannot see resource use is not managing.
//
// Two rules shape everything here, and both exist because the alternative
// misleads:
//
//   - A rate is a delta. Cumulative CPU time divided by process age is a
//     lifetime average -- what `ps %cpu` reports on Linux, and what anyone
//     dividing `ps -o time` by uptime computes -- and it is why `ps` and `top`
//     can disagree by half on the same PID. Every percentage this package
//     produces comes from two cumulative readings and the wall time between
//     them, so a process that burned a core an hour ago and is asleep now
//     reads as asleep now.
//   - Unknown is not zero. An unreadable process, a session with no process,
//     and a first sample with nothing to subtract from are all reported as
//     absent rather than as 0, so a reader can never mistake "Sessions does
//     not know" for "this costs nothing".
package resource

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is how often the daemon takes a whole-machine sample.
//
// It is deliberately much slower than the activity tick it rides on. A sample
// costs one pass over the process table regardless of how many sessions exist,
// and a CPU percentage measured over a few hundred milliseconds is mostly
// scheduling noise. Five seconds is short enough that a listing is never more
// than one tick stale and long enough that the measurement means something.
const DefaultInterval = 5 * time.Second

// runnerProcessNames are the executable names a Sessions runner runs under.
// Used only to decide where a session's process tree starts; see Table.Root.
var runnerProcessNames = map[string]struct{}{
	"sessions-runner":     {},
	"sessions-runner.exe": {},
}

// Process is one process's cost at one instant, exactly as the OS reported it.
type Process struct {
	PID  int
	PPID int
	// Name is the executable name the kernel records. It is short and may be
	// truncated (macOS caps it at 16 bytes, Linux at 15); nothing here parses
	// meaning out of it beyond recognising a Sessions runner.
	Name string
	// RSSBytes is resident set size: physical memory the process currently
	// holds. It is the number Activity Monitor and `ps rss` show, and unlike
	// virtual size it is what actually competes for RAM.
	RSSBytes uint64
	// CPUTime is user+system CPU consumed since the process started. It is a
	// counter, not a rate. Turning it into a rate needs a second reading and
	// the wall time between them, which is what CPUPercent does.
	CPUTime time.Duration
}

// Enumerator returns one whole-machine process snapshot.
//
// The interface is whole-machine rather than per-PID on purpose. The cost of
// answering "what does this session cost" has to stay flat as sessions
// multiply, and one pass over the process table costs the same for two
// sessions as for two hundred. It is also the seam the tests inject a
// fabricated process table through.
type Enumerator interface {
	Enumerate() ([]Process, error)
}

// Table indexes one snapshot for parent and child lookups.
type Table struct {
	byPID    map[int]Process
	children map[int][]int
}

// NewTable indexes a snapshot. Later entries for a repeated PID win, which
// only happens if an enumerator reports a PID twice.
func NewTable(processes []Process) *Table {
	table := &Table{byPID: make(map[int]Process, len(processes)), children: make(map[int][]int)}
	for _, process := range processes {
		table.byPID[process.PID] = process
	}
	for _, process := range processes {
		if process.PPID == process.PID || process.PPID == 0 {
			continue
		}
		table.children[process.PPID] = append(table.children[process.PPID], process.PID)
	}
	for parent := range table.children {
		sort.Ints(table.children[parent])
	}
	return table
}

// Process returns one entry from the snapshot.
func (t *Table) Process(pid int) (Process, bool) {
	if t == nil {
		return Process{}, false
	}
	process, ok := t.byPID[pid]
	return process, ok
}

// Root resolves the process a session's cost should be measured from.
//
// The daemon is told different PIDs for different session kinds and the
// difference matters. A structured runner reports its own PID
// (cmd/sessions-runner/claude_p.go, codex_app.go), so the PID in SessionInfo
// is already the top of the session's tree. A PTY runner reports the PID of
// the process it put on the terminal (cmd/sessions-runner/main.go) -- the
// provider, not the runner. Measuring a PTY session from there leaves the
// runner's own resident memory and CPU attributed to nobody, which on this
// author's machine is tens of megabytes and minutes of CPU per session.
//
// Climbing one level when the parent is a Sessions runner puts both kinds on
// the same footing: the tree Sessions started, measured from its top. The
// climb is keyed on the runner's executable name, which is the same signal
// discovery already uses to tell a live runner from a recycled PID
// (internal/session/process_unix.go). Getting it wrong costs one runner
// process of attribution in either direction; it can never merge two sessions,
// because a runner hosts exactly one.
func (t *Table) Root(pid int) int {
	if t == nil {
		return pid
	}
	process, ok := t.byPID[pid]
	if !ok {
		return pid
	}
	if isRunnerProcess(process) {
		return pid
	}
	parent, ok := t.byPID[process.PPID]
	if ok && isRunnerProcess(parent) {
		return parent.PID
	}
	return pid
}

func isRunnerProcess(process Process) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(process.Name)))
	_, ok := runnerProcessNames[name]
	return ok
}

// maxTreeDepth bounds the descendant walk. A process table is a forest and
// cannot contain a cycle, but it is read without a lock while processes are
// being created and reaped, so a torn read could in principle produce one.
// Bounding the walk means a bad read costs a wrong number for one tick rather
// than a hung daemon.
const maxTreeDepth = 32

// Tree returns the root and every descendant of it in the snapshot.
//
// What this covers: a PTY session's runner, the provider it put on the
// terminal, and everything the provider spawned -- shells, language servers,
// MCP servers, the compilers an agent runs -- to any depth.
//
// What it misses, and cannot recover: a process that has been reparented. When
// a session's intermediate process exits, its surviving children are adopted
// by init and leave the tree, so their cost stops being charged to the session
// that created them. That is a real limit of parent-pointer accounting, not a
// bug in the walk; the honest alternative would be a process group or cgroup,
// which Sessions does not create.
func (t *Table) Tree(root int) []Process {
	if t == nil {
		return nil
	}
	if _, ok := t.byPID[root]; !ok {
		return nil
	}
	seen := map[int]struct{}{root: {}}
	collected := []Process{t.byPID[root]}
	frontier := []int{root}
	for depth := 0; depth < maxTreeDepth && len(frontier) > 0; depth++ {
		next := make([]int, 0, len(frontier))
		for _, parent := range frontier {
			for _, child := range t.children[parent] {
				if _, already := seen[child]; already {
					continue
				}
				seen[child] = struct{}{}
				collected = append(collected, t.byPID[child])
				next = append(next, child)
			}
		}
		frontier = next
	}
	return collected
}

// CPUPercent turns two cumulative CPU readings, taken elapsed wall time apart,
// into a percentage of one core. 100 means one core saturated; a tree using
// four cores reads as 400.
//
// The second return is false whenever the pair cannot support a rate, and the
// caller must render that as unknown rather than as zero:
//
//   - elapsed is zero or negative: there is no wall time to divide by. Two
//     readings from the same instant say nothing about speed.
//   - current is below previous: the counter went backwards. A cumulative
//     counter cannot decrease, so this is a process that exited and had its
//     PID reused, or a tree whose membership changed underneath the caller.
//     The honest answer is "unknown", not a negative percentage and not zero.
//
// A first sample has no previous reading at all and so never reaches here; the
// tracker reports it as unknown until a second sample exists.
func CPUPercent(previous, current, elapsed time.Duration) (float64, bool) {
	if elapsed <= 0 {
		return 0, false
	}
	if current < previous {
		return 0, false
	}
	return float64(current-previous) / float64(elapsed) * 100, true
}

// Restate makes two tree readings subtractable.
//
// A process tree is not a fixed set: children are spawned and reaped between
// samples, so subtracting one tree total from another compares different
// populations and produces nonsense -- a large child exiting looks like
// negative CPU. Restate reprojects the previous reading onto the PIDs the
// current reading actually has, so the difference is the sum of each surviving
// process's own delta plus the full cost of processes born during the window.
//
// A process that vanished during the window takes the CPU it burned with it.
// That is under-counting, and it is the direction to err in: over-counting a
// dead process's lifetime CPU against a five-second window is exactly the
// lifetime-average mistake this package exists to avoid.
func Restate(previous, current map[int]time.Duration) (previousTotal, currentTotal time.Duration) {
	for pid, cpu := range current {
		currentTotal += cpu
		previousTotal += previous[pid]
	}
	return previousTotal, currentTotal
}

// Sample is one session's measured cost.
//
// The two "known" flags are separate because they fail independently: a tree
// that was found always has a memory figure, but its CPU rate needs a previous
// sample of the same tree and so is unknown for one tick after the session
// appears.
type Sample struct {
	// Known is false when the session has no process in the snapshot at all --
	// it never started, it exited, or the OS refused to describe it. Readers
	// must render this as unknown, never as 0.
	Known bool
	// RSSBytes is the resident memory of the whole tree. Valid only when Known.
	RSSBytes uint64
	// Processes is how many processes contributed, so a reader can see whether
	// a number covers a whole tree or a lone runner.
	Processes int
	// CPUPercent is percent of one core over the interval ending at At. Valid
	// only when CPUKnown.
	CPUPercent float64
	CPUKnown   bool
	// At is when the sample was taken. It is carried rather than assumed
	// current because a reader that renders a sample minutes after the daemon
	// took it is entitled to know that, and a listing that hides its own
	// staleness is the same lie as reporting unknown as zero.
	At time.Time
}

// Tracker turns successive whole-machine snapshots into per-session samples.
// It is safe for concurrent use.
type Tracker struct {
	enumerator Enumerator
	now        func() time.Time

	mu       sync.Mutex
	previous map[string]reading
}

// reading is the per-PID CPU counter of one session's tree at one instant.
// Per-PID rather than a single total, because Restate needs the breakdown to
// tell a child exiting from a counter going backwards.
type reading struct {
	at  time.Time
	cpu map[int]time.Duration
}

// NewTracker builds a tracker over an enumerator. now is injectable so tests
// can control elapsed time exactly; nil means time.Now.
func NewTracker(enumerator Enumerator, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{enumerator: enumerator, now: now, previous: make(map[string]reading)}
}

// Sample measures every session in roots against one fresh snapshot.
//
// roots maps session id to the PID the daemon was told about. Every id in
// roots gets an entry in the result, including sessions with no live process:
// their sample is simply not Known, which is the answer, and dropping them
// would leave a reader unable to tell "no process" from "not asked".
//
// Sessions absent from roots are forgotten, so a tracker cannot accumulate
// state for sessions that have ended.
func (t *Tracker) Sample(roots map[string]int) (map[string]Sample, error) {
	processes, err := t.enumerator.Enumerate()
	if err != nil {
		return nil, err
	}
	now := t.now()
	table := NewTable(processes)

	samples := make(map[string]Sample, len(roots))
	current := make(map[string]reading, len(roots))

	t.mu.Lock()
	defer t.mu.Unlock()
	for id, pid := range roots {
		if pid <= 0 {
			samples[id] = Sample{At: now}
			continue
		}
		tree := table.Tree(table.Root(pid))
		if len(tree) == 0 {
			samples[id] = Sample{At: now}
			continue
		}
		sample := Sample{Known: true, Processes: len(tree), At: now}
		cpu := make(map[int]time.Duration, len(tree))
		for _, process := range tree {
			sample.RSSBytes += process.RSSBytes
			cpu[process.PID] = process.CPUTime
		}
		if last, ok := t.previous[id]; ok {
			previousTotal, currentTotal := Restate(last.cpu, cpu)
			sample.CPUPercent, sample.CPUKnown = CPUPercent(previousTotal, currentTotal, now.Sub(last.at))
		}
		samples[id] = sample
		current[id] = reading{at: now, cpu: cpu}
	}
	t.previous = current
	return samples, nil
}
