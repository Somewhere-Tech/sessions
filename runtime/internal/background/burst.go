package background

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/cpuprofile"
)

// The daemon profiles its own bursts, because nobody else is there when they
// happen.
//
// Measured on the Mini, 11 September: after `[startup] 47 sessions in 11.3s`,
// sessionsd ran about 150 s at 100-170% CPU. The named passes accounted for
// roughly 16 CPU-seconds of it — a tenth. The rest was work nobody had a name
// for, on a machine where `doctor --cpu-profile` refused because production
// daemons had profiling off, and where by the time anybody could have turned it
// on the burst was over.
//
// So the daemon watches its own CPU and, when it stays busy for long enough to
// be a burst rather than a request, takes one profile and writes down what the
// top frames were. It keeps the newest few files and logs one line. The line
// carries symbol names and percentages: nothing about anybody's sessions.

const (
	// burstSample is how often process CPU is read. Getrusage is cheap and this
	// is not a measurement of anything fast.
	burstSample = 5 * time.Second
	// burstSustain is how long CPU must stay high before this is a burst. A
	// listing that takes four seconds is not a burst; four seconds of a listing
	// every five seconds for twenty is.
	burstSustain = 20 * time.Second
	// burstDuration is the profile's own length.
	burstDuration = 30 * time.Second
	// burstCooldown keeps a daemon that is busy for an hour from writing a
	// profile every thirty seconds.
	burstCooldown = 10 * time.Minute
	// burstThreshold is CPU per wall second — 0.8 of one core — sustained.
	burstThreshold = 0.8
	// burstKeep and burstMaxBytes bound what is left on somebody's disk.
	burstKeep     = 3
	burstMaxBytes = 96 << 20
)

// BurstOptions is what a daemon tells the watcher about itself. Everything is
// optional: the zero value profiles a real process with the constants above.
type BurstOptions struct {
	// Dir is where profiles are written, usually <state>/profiles.
	Dir string
	// Ready reports whether startup has finished. A daemon at 100% CPU while it
	// is still loading is doing the work it said it was doing, and already logs
	// a line about it.
	Ready func() bool
	Logf  func(string, ...any)
	// The rest exist so a test can make a burst happen in milliseconds.
	Now       func() time.Time
	CPU       func() (time.Duration, bool)
	Profile   func(io.Writer, time.Duration) error
	Sample    time.Duration
	Sustain   time.Duration
	Duration  time.Duration
	Cooldown  time.Duration
	Threshold float64
}

func (o BurstOptions) withDefaults() BurstOptions {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CPU == nil {
		o.CPU = processCPU
	}
	if o.Profile == nil {
		o.Profile = captureCPUProfile
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Sample <= 0 {
		o.Sample = burstSample
	}
	if o.Sustain <= 0 {
		o.Sustain = burstSustain
	}
	if o.Duration <= 0 {
		o.Duration = burstDuration
	}
	if o.Cooldown <= 0 {
		o.Cooldown = burstCooldown
	}
	if o.Threshold <= 0 {
		o.Threshold = burstThreshold
	}
	return o
}

// WatchBurst samples this process's CPU until ctx ends, and profiles a burst
// when it sees one. It blocks; callers run it in a goroutine.
func WatchBurst(ctx context.Context, options BurstOptions) {
	options = options.withDefaults()
	if options.Dir == "" {
		return
	}
	ticker := time.NewTicker(options.Sample)
	defer ticker.Stop()
	lastCPU, ok := options.CPU()
	if !ok {
		// A platform that cannot report its own CPU cannot notice a burst. Say
		// nothing rather than profile on a guess.
		return
	}
	lastAt := options.Now()
	var hotSince, lastCapture time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cpu, ok := options.CPU()
		if !ok {
			return
		}
		now := options.Now()
		elapsed := now.Sub(lastAt)
		used := cpu - lastCPU
		lastCPU, lastAt = cpu, now
		if elapsed <= 0 {
			continue
		}
		if options.Ready != nil && !options.Ready() {
			hotSince = time.Time{}
			continue
		}
		if float64(used)/float64(elapsed) < options.Threshold {
			hotSince = time.Time{}
			continue
		}
		if hotSince.IsZero() {
			hotSince = now
			continue
		}
		if now.Sub(hotSince) < options.Sustain {
			continue
		}
		if !lastCapture.IsZero() && now.Sub(lastCapture) < options.Cooldown {
			continue
		}
		lastCapture = now
		hotSince = time.Time{}
		captureBurst(options, now)
		// The profile itself took wall time; start the next window from here.
		if cpu, ok := options.CPU(); ok {
			lastCPU, lastAt = cpu, options.Now()
		}
	}
}

func captureBurst(options BurstOptions, at time.Time) {
	if err := os.MkdirAll(options.Dir, 0o700); err != nil {
		options.Logf("[burst] cannot write a profile to %s: %v", options.Dir, err)
		return
	}
	path := filepath.Join(options.Dir, fmt.Sprintf("burst-%s.pprof", at.UTC().Format("20060102T150405Z")))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		options.Logf("[burst] cannot write a profile to %s: %v", path, err)
		return
	}
	profileErr := options.Profile(file, options.Duration)
	closeErr := file.Close()
	if profileErr != nil || closeErr != nil {
		options.Logf("[burst] profile failed: %v", errorsJoin(profileErr, closeErr))
		return
	}
	options.Logf("[burst] %s profile: %s (saved to %s)",
		round(options.Duration), describeProfile(path), path)
	pruneProfiles(options)
}

// describeProfile is the answer in one line, or an honest failure to read it.
func describeProfile(path string) string {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "written, but could not be read back: " + err.Error()
	}
	frames, err := cpuprofile.TopFrames(encoded, 3)
	if err != nil {
		return "written, but could not be summarised: " + err.Error()
	}
	if len(frames) == 0 {
		return "no samples — the burst ended before the profile did"
	}
	return "top frames — " + cpuprofile.Line(frames)
}

// pruneProfiles keeps the newest few and a bounded total, because a diagnostic
// that fills somebody's disk is a fault of its own.
func pruneProfiles(options BurstOptions) {
	entries, err := os.ReadDir(options.Dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "burst-") && strings.HasSuffix(entry.Name(), ".pprof") {
			names = append(names, entry.Name())
		}
	}
	// Names carry a UTC timestamp, so newest last by name.
	sort.Strings(names)
	var total int64
	for index := len(names) - 1; index >= 0; index-- {
		path := filepath.Join(options.Dir, names[index])
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		kept := len(names) - 1 - index
		if kept >= burstKeep || total+info.Size() > burstMaxBytes {
			_ = os.Remove(path)
			continue
		}
		total += info.Size()
	}
}

func captureCPUProfile(writer io.Writer, duration time.Duration) error {
	if err := pprof.StartCPUProfile(writer); err != nil {
		return err
	}
	time.Sleep(duration)
	pprof.StopCPUProfile()
	return nil
}

func errorsJoin(first, second error) error {
	if first != nil {
		return first
	}
	return second
}
