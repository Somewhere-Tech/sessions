package background

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// From the Mini, 11 September: after `[startup] 47 sessions in 11.3s` the daemon
// ran about 150 s at 100-170% CPU, and roughly nine tenths of it belonged to no
// named pass. `doctor --cpu-profile` refused because production daemons had
// profiling off, and by the time anybody could turn it on the burst was over.
// This is the daemon noticing it itself: a real goroutine spins, the watcher
// profiles the process, and the line it logs names the spinning function.
func TestABurstIsProfiledOnceAndTheLineNamesTheSpinningFrame(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	var linesMu sync.Mutex

	stopSpin := make(chan struct{})
	var spinners sync.WaitGroup
	for range 2 {
		spinners.Add(1)
		go func() {
			defer spinners.Done()
			spinUntilBurstEnds(stopSpin)
		}()
	}
	defer func() { close(stopSpin); spinners.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		WatchBurst(ctx, BurstOptions{
			Dir:      dir,
			Ready:    func() bool { return true },
			Sample:   50 * time.Millisecond,
			Sustain:  100 * time.Millisecond,
			Duration: 300 * time.Millisecond,
			Cooldown: 20 * time.Second,
			Logf: func(format string, args ...any) {
				linesMu.Lock()
				defer linesMu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
	}()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		linesMu.Lock()
		count := len(lines)
		linesMu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Give the watcher room to capture a second time if it were going to.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	linesMu.Lock()
	defer linesMu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("a sustained burst produced %d lines: %#v", len(lines), lines)
	}
	line := lines[0]
	t.Log(line)
	if !strings.Contains(line, "[burst]") || !strings.Contains(line, "top frames") {
		t.Fatalf("the burst line does not summarise the profile: %q", line)
	}
	if !strings.Contains(line, "spinUntilBurstEnds") {
		t.Fatalf("the burst line does not name the function that was spinning: %q", line)
	}
	if !strings.Contains(line, "saved to "+dir) {
		t.Fatalf("the burst line does not say where the profile went: %q", line)
	}
	profiles := profileFiles(t, dir)
	if len(profiles) != 1 {
		t.Fatalf("captured %d profiles, want exactly one", len(profiles))
	}
}

// spinUntilBurstEnds is the fixture's own hot loop, named so the assertion can
// look for it by name in the profile the daemon takes of itself.
func spinUntilBurstEnds(stop <-chan struct{}) {
	var sink uint64
	for {
		select {
		case <-stop:
			_ = sink
			return
		default:
		}
		for index := uint64(0); index < 2_000_000; index++ {
			sink = sink*1_664_525 + index
		}
	}
}

// A daemon that is still loading is doing the work it said it was doing, and
// already logs a line about it.
func TestNoProfileWhileTheDaemonIsStillLoading(t *testing.T) {
	dir := t.TempDir()
	var captures atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cpu := fakeCPU(time.Second)
	WatchBurst(ctx, BurstOptions{
		Dir:      dir,
		Ready:    func() bool { return false },
		Now:      time.Now,
		CPU:      cpu,
		Sample:   10 * time.Millisecond,
		Sustain:  20 * time.Millisecond,
		Duration: time.Millisecond,
		Profile: func(_ io.Writer, _ time.Duration) error {
			captures.Add(1)
			return nil
		},
	})
	if captures.Load() != 0 {
		t.Fatalf("profiled %d times while the daemon was still loading", captures.Load())
	}
}

// One capture per cooldown, however long the machine stays busy.
func TestASustainedBurstIsProfiledOncePerCooldown(t *testing.T) {
	dir := t.TempDir()
	var captures atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	WatchBurst(ctx, BurstOptions{
		Dir:      dir,
		Ready:    func() bool { return true },
		CPU:      fakeCPU(time.Second),
		Sample:   10 * time.Millisecond,
		Sustain:  20 * time.Millisecond,
		Duration: time.Millisecond,
		Cooldown: time.Hour,
		Profile: func(writer io.Writer, _ time.Duration) error {
			captures.Add(1)
			_, _ = writer.Write([]byte("not a profile"))
			return nil
		},
	})
	if got := captures.Load(); got != 1 {
		t.Fatalf("a burst lasting three seconds was profiled %d times, want 1", got)
	}
	// The file is kept even when it cannot be summarised, and the line says so
	// rather than claiming an answer.
	if files := profileFiles(t, dir); len(files) != 1 {
		t.Fatalf("kept %d profiles, want 1", len(files))
	}
}

// Three is what is kept; a machine that bursts all week does not fill a disk.
func TestOnlyTheNewestProfilesAreKept(t *testing.T) {
	dir := t.TempDir()
	for index := range 6 {
		name := filepath.Join(dir, fmt.Sprintf("burst-2026091%dT000000Z.pprof", index))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneProfiles(BurstOptions{Dir: dir})
	files := profileFiles(t, dir)
	if len(files) != burstKeep {
		t.Fatalf("kept %d profiles, want %d: %#v", len(files), burstKeep, files)
	}
	if files[len(files)-1] != "burst-20260915T000000Z.pprof" {
		t.Fatalf("kept %#v, want the newest", files)
	}
}

func profileFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "burst-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// fakeCPU reports a process burning one core per wall second, without burning
// one: the sampling arithmetic is what these tests are about.
func fakeCPU(perSecond time.Duration) func() (time.Duration, bool) {
	start := time.Now()
	return func() (time.Duration, bool) {
		return time.Duration(float64(time.Since(start)) * float64(perSecond) / float64(time.Second)), true
	}
}
