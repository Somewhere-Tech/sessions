// Package waitcond implements observation-only conditions for sessions wait.
// Filesystem notifications are latency hints; every condition retains a
// polling path so replacement, deletion, and missed notifications are safe.
package waitcond

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	commitPollInterval = 5 * time.Second
	filePollInterval   = 250 * time.Millisecond
	idlePollInterval   = 100 * time.Millisecond

	// MaxFileRead bounds both memory use and bytes read per file observation.
	// For larger files, the newest bytes are inspected so appended sentinels
	// remain useful without unbounded reads.
	MaxFileRead = 8 * 1024 * 1024
)

// Kind identifies the observation that satisfied a wait.
type Kind string

const (
	CommitKind       Kind = "commit"
	FileContainsKind Kind = "file_contains"
	IdleStableKind   Kind = "idle_stable"
)

// Result contains the union of facts produced by the supported conditions.
// Callers choose the public output shape appropriate to Kind.
type Result struct {
	Kind             Kind
	Session          string
	Cwd              string
	Elapsed          time.Duration
	Baseline         string
	Commit           string
	Subject          string
	HistoryRewritten bool
	File             string
	Contains         string
	Stable           time.Duration
	Source           string
}

// Condition is one independently waitable observation.
type Condition interface {
	Wait(context.Context) (Result, error)
}

// PreconditionError identifies invalid or unavailable observation inputs.
// The CLI maps it to exit status 1 rather than a timeout.
type PreconditionError struct{ Err error }

func (e *PreconditionError) Error() string { return e.Err.Error() }
func (e *PreconditionError) Unwrap() error { return e.Err }

func precondition(format string, args ...any) error {
	return &PreconditionError{Err: fmt.Errorf(format, args...)}
}

// IsPrecondition reports whether err is a condition setup/runtime failure
// rather than cancellation or deadline expiry.
func IsPrecondition(err error) bool {
	var target *PreconditionError
	return errors.As(err, &target)
}

// WaitAny returns the first satisfied condition. When every condition fails,
// their errors are joined. The shared context cancels losing observers.
func WaitAny(ctx context.Context, conditions []Condition) (Result, error) {
	if len(conditions) == 0 {
		return Result{}, precondition("no wait conditions")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, len(conditions))
	var group sync.WaitGroup
	group.Add(len(conditions))
	for _, condition := range conditions {
		condition := condition
		go func() {
			defer group.Done()
			result, err := condition.Wait(ctx)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	go func() {
		group.Wait()
		close(outcomes)
	}()

	errorsSeen := make([]error, 0, len(conditions))
	for outcome := range outcomes {
		if outcome.err == nil {
			cancel()
			return outcome.result, nil
		}
		if errors.Is(outcome.err, context.Canceled) || errors.Is(outcome.err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
		}
		errorsSeen = append(errorsSeen, outcome.err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{}, errors.Join(errorsSeen...)
}

type commitCondition struct {
	session  string
	cwd      string
	baseline string
	logPath  string
	watch    watchFactory
	ticker   tickerFactory
	checked  func()
}

type conditionTicker struct {
	ticks <-chan time.Time
	stop  func()
}

type watchFactory func(string) (<-chan struct{}, func())
type tickerFactory func(time.Duration) conditionTicker

func startTicker(interval time.Duration) conditionTicker {
	ticker := time.NewTicker(interval)
	return conditionTicker{ticks: ticker.C, stop: ticker.Stop}
}

// NewCommit snapshots HEAD immediately. A later HEAD difference satisfies the
// condition, including a backwards or sideways history rewrite.
func NewCommit(ctx context.Context, session, cwd string) (Condition, error) {
	cwd, err := cleanDirectory(cwd)
	if err != nil {
		return nil, err
	}
	baseline, err := gitOutput(ctx, cwd, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, precondition("session %s cwd is not a git repository with a commit: %v", session, err)
	}
	gitDir, err := gitOutput(ctx, cwd, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, precondition("resolve git directory for session %s: %v", session, err)
	}
	return &commitCondition{
		session: session, cwd: cwd, baseline: baseline,
		logPath: filepath.Join(gitDir, "logs", "HEAD"),
		watch:   watchParent,
		ticker:  startTicker,
	}, nil
}

func (condition *commitCondition) Wait(ctx context.Context) (Result, error) {
	started := time.Now()
	wake, closeWake := condition.watch(condition.logPath)
	defer closeWake()
	ticker := condition.ticker(commitPollInterval)
	defer ticker.stop()
	for {
		result, satisfied, err := condition.check(ctx)
		if err != nil {
			return Result{}, err
		}
		if satisfied {
			result.Elapsed = time.Since(started)
			return result, nil
		}
		if condition.checked != nil {
			condition.checked()
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-ticker.ticks:
		case <-wake:
		}
	}
}

func (condition *commitCondition) check(ctx context.Context) (Result, bool, error) {
	current, err := gitOutput(ctx, condition.cwd, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Result{}, false, precondition("read HEAD for session %s: %v", condition.session, err)
	}
	if current == condition.baseline {
		return Result{}, false, nil
	}
	rewritten, err := historyRewritten(ctx, condition.cwd, condition.baseline, current)
	if err != nil {
		return Result{}, false, precondition("compare git history for session %s: %v", condition.session, err)
	}
	subject, err := gitOutput(ctx, condition.cwd, "show", "-s", "--format=%s", current)
	if err != nil {
		return Result{}, false, precondition("read commit subject for session %s: %v", condition.session, err)
	}
	return Result{
		Kind: CommitKind, Session: condition.session, Cwd: condition.cwd,
		Baseline: condition.baseline, Commit: current, Subject: subject,
		HistoryRewritten: rewritten,
	}, true, nil
}

func historyRewritten(ctx context.Context, cwd, baseline, current string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", cwd, "merge-base", "--is-ancestor", baseline, current)
	err := command.Run()
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	arguments := append([]string{"-C", cwd}, args...)
	command := exec.CommandContext(ctx, "git", arguments...)
	encoded, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message := strings.TrimSpace(string(exit.Stderr))
			if message != "" {
				return "", fmt.Errorf("%s", message)
			}
		}
		return "", err
	}
	return strings.TrimSpace(string(encoded)), nil
}

type fileContainsCondition struct {
	session string
	cwd     string
	path    string
	needle  []byte
	literal string
	watch   watchFactory
	ticker  tickerFactory
}

// NewFileContains creates a literal-byte file condition. Relative paths are
// rooted at the session cwd.
func NewFileContains(session, cwd, path, literal string) (Condition, error) {
	cwd, err := cleanDirectory(cwd)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, precondition("file path is empty")
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cwd, resolved)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, precondition("resolve file %q: %v", path, err)
	}
	needle := []byte(literal)
	if len(needle) > MaxFileRead {
		return nil, precondition("literal is larger than the %d-byte file read cap", MaxFileRead)
	}
	return &fileContainsCondition{
		session: session, cwd: cwd, path: resolved, needle: needle, literal: literal,
		watch: watchParent, ticker: startTicker,
	}, nil
}

func (condition *fileContainsCondition) Wait(ctx context.Context) (Result, error) {
	started := time.Now()
	wake, closeWake := condition.watch(condition.path)
	defer closeWake()
	ticker := condition.ticker(filePollInterval)
	defer ticker.stop()
	for {
		satisfied, err := fileContains(condition.path, condition.needle)
		if err != nil {
			return Result{}, precondition("read %s: %v", condition.path, err)
		}
		if satisfied {
			return Result{
				Kind: FileContainsKind, Session: condition.session, Cwd: condition.cwd,
				File: condition.path, Contains: condition.literal, Elapsed: time.Since(started),
			}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-ticker.ticks:
		case <-wake:
		}
	}
}

func fileContains(path string, needle []byte) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, precondition("not a regular file")
	}
	start := int64(0)
	if info.Size() > MaxFileRead {
		start = info.Size() - MaxFileRead
	}
	reader := io.NewSectionReader(file, start, min(info.Size()-start, int64(MaxFileRead)))
	encoded, err := io.ReadAll(reader)
	if err != nil {
		return false, err
	}
	return bytes.Contains(encoded, needle), nil
}

// IdleSample is one daemon observation of the working classifier.
type IdleSample struct {
	Working bool
	Source  string
}

// IdleObserver fetches the daemon's current working flag. Transient errors are
// treated as missing evidence and reset the continuous-idle window.
type IdleObserver func(context.Context) (IdleSample, error)

type idleStableCondition struct {
	session string
	cwd     string
	stable  time.Duration
	observe IdleObserver
	now     func() time.Time
	ticker  tickerFactory
}

// NewIdleStable creates a continuous idle-observation condition.
func NewIdleStable(session, cwd string, stable time.Duration, observe IdleObserver) (Condition, error) {
	if stable <= 0 {
		return nil, precondition("idle-stable duration must be greater than zero")
	}
	if observe == nil {
		return nil, precondition("idle observer is required")
	}
	return &idleStableCondition{
		session: session, cwd: cwd, stable: stable, observe: observe,
		now: time.Now, ticker: startTicker,
	}, nil
}

func (condition *idleStableCondition) Wait(ctx context.Context) (Result, error) {
	started := condition.now()
	poll := idlePollInterval
	if quarter := condition.stable / 4; quarter > 0 && quarter < poll {
		poll = quarter
	}
	if poll < 10*time.Millisecond {
		poll = 10 * time.Millisecond
	}
	ticker := condition.ticker(poll)
	defer ticker.stop()
	var idleSince time.Time
	for {
		now := condition.now()
		sample, err := condition.observe(ctx)
		if err != nil {
			if IsPrecondition(err) {
				return Result{}, err
			}
			idleSince = time.Time{}
		} else if sample.Working {
			idleSince = time.Time{}
		} else {
			if idleSince.IsZero() {
				idleSince = now
			}
			if now.Sub(idleSince) >= condition.stable {
				return Result{
					Kind: IdleStableKind, Session: condition.session, Cwd: condition.cwd,
					Stable: condition.stable, Source: sample.Source, Elapsed: condition.now().Sub(started),
				}, nil
			}
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-ticker.ticks:
		}
	}
}

func cleanDirectory(path string) (string, error) {
	if path == "" {
		return "", precondition("session cwd is empty")
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", precondition("resolve session cwd: %v", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", precondition("inspect session cwd %s: %v", resolved, err)
	}
	if !info.IsDir() {
		return "", precondition("session cwd is not a directory: %s", resolved)
	}
	return resolved, nil
}

// watchParent returns a coalesced wake channel. Failure to establish a native
// watch is harmless because callers always retain their polling timer.
func watchParent(path string) (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return wake, func() { close(done) }
	}
	parent := filepath.Dir(path)
	if err := watcher.Add(parent); err != nil {
		_ = watcher.Close()
		return wake, func() { close(done) }
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	return wake, func() {
		once.Do(func() {
			close(done)
			_ = watcher.Close()
		})
	}
}
