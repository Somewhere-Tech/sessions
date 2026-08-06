package filelock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file spawn real processes on purpose. Two goroutines in one
// address space can be serialised by the Go scheduler, by a mutex a future
// refactor adds, or by chance, so an in-process test of a cross-process lock
// proves nothing about the case this package exists for: a daemon and a runner
// that launchd started, which share no memory at all. Each helper below is a
// re-invocation of this test binary with an env marker, so the only thing the
// two sides share is the filesystem.

const (
	helperRoleEnv       = "SESSIONS_FILELOCK_HELPER_ROLE"
	helperLockPathEnv   = "SESSIONS_FILELOCK_LOCK_PATH"
	helperCounterEnv    = "SESSIONS_FILELOCK_COUNTER_PATH"
	helperIterationsEnv = "SESSIONS_FILELOCK_ITERATIONS"
	helperUnlockedEnv   = "SESSIONS_FILELOCK_WITHOUT_LOCK"

	holdRole      = "hold"
	incrementRole = "increment"

	// helperReady is written to stdout once a helper has reached its
	// synchronisation point, and helperStart is the single byte the parent
	// writes to stdin to release it. Both directions are explicit handshakes
	// rather than sleeps, because a sleep long enough to be reliable on a
	// loaded machine is long enough to make the suite useless.
	helperReady = "ready\n"
	helperStart = 'g'

	// testBudget bounds every wait in this file. No test here performs a bare
	// channel receive: a test that hangs reports nothing, and this repository
	// has been bitten by that before.
	testBudget = 60 * time.Second

	// helperBudget is the helper's own watchdog. A helper must never outlive
	// the test that spawned it, even if that test dies without closing the
	// handshake pipe.
	helperBudget = 90 * time.Second

	// counterHoldWindow is the gap a counter helper leaves between reading the
	// counter and writing it back. It is the analogue of the JSON decode and
	// re-encode the real read-modify-write performs, and it makes an unlocked
	// run lose updates every time instead of once in some thousands of runs. It
	// does not create the race; it only makes the existing window observable.
	counterHoldWindow = 2 * time.Millisecond
)

// TestFileLockHelperProcess is not a test. It is the body of every helper
// process the tests below spawn, selected by SESSIONS_FILELOCK_HELPER_ROLE.
func TestFileLockHelperProcess(t *testing.T) {
	role := os.Getenv(helperRoleEnv)
	if role == "" {
		t.Skip("helper process entry point for the cross-process lock tests")
	}
	watchdog := time.AfterFunc(helperBudget, func() {
		fmt.Fprintln(os.Stderr, "helper: watchdog expired, the parent never released it")
		os.Exit(9)
	})
	defer watchdog.Stop()

	switch role {
	case holdRole:
		runHoldHelper()
	case incrementRole:
		runIncrementHelper()
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown role %q\n", role)
		os.Exit(2)
	}
}

// runHoldHelper takes the lock, tells the parent it has it, and holds it until
// the parent says otherwise. It is the "other process" every blocking and
// cancellation assertion in this file is measured against.
func runHoldHelper() {
	ctx, cancel := context.WithTimeout(context.Background(), helperBudget)
	defer cancel()
	lock, err := Acquire(ctx, os.Getenv(helperLockPathEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: Acquire: %v\n", err)
		os.Exit(3)
	}
	if _, err := io.WriteString(os.Stdout, helperReady); err != nil {
		fmt.Fprintf(os.Stderr, "helper: announce readiness: %v\n", err)
		os.Exit(4)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		fmt.Fprintf(os.Stderr, "helper: read the release byte: %v\n", err)
		os.Exit(5)
	}
	if err := lock.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "helper: Release: %v\n", err)
		os.Exit(6)
	}
}

// runIncrementHelper performs the read-modify-write this package exists to
// serialise: read a number from a shared file, add one, write it back. The
// write is a temp file plus a rename, exactly as state.WriteMetadata writes a
// session document, so a torn read is impossible and the only way the total can
// come out wrong is a lost update.
func runIncrementHelper() {
	lockPath := os.Getenv(helperLockPathEnv)
	counterPath := os.Getenv(helperCounterEnv)
	withoutLock := os.Getenv(helperUnlockedEnv) == "1"
	iterations, err := strconv.Atoi(os.Getenv(helperIterationsEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: parse iteration count: %v\n", err)
		os.Exit(2)
	}

	if _, err := io.WriteString(os.Stdout, helperReady); err != nil {
		fmt.Fprintf(os.Stderr, "helper: announce readiness: %v\n", err)
		os.Exit(4)
	}
	// Every helper starts incrementing on the same byte so their windows really
	// do overlap. Without this the processes would be staggered by however long
	// each took to start, and a fast machine could run them one after another.
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		fmt.Fprintf(os.Stderr, "helper: read the start byte: %v\n", err)
		os.Exit(5)
	}

	for iteration := 0; iteration < iterations; iteration++ {
		var lock *Lock
		if !withoutLock {
			ctx, cancel := context.WithTimeout(context.Background(), helperBudget)
			lock, err = Acquire(ctx, lockPath)
			// The context governs acquisition only; once the lock is held it
			// has no further say, so cancelling here releases the timer without
			// touching the lock.
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "helper: Acquire on iteration %d: %v\n", iteration, err)
				os.Exit(3)
			}
		}
		value, err := readCounter(counterPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: read counter on iteration %d: %v\n", iteration, err)
			os.Exit(7)
		}
		time.Sleep(counterHoldWindow)
		if err := writeCounter(counterPath, value+1); err != nil {
			fmt.Fprintf(os.Stderr, "helper: write counter on iteration %d: %v\n", iteration, err)
			os.Exit(8)
		}
		if lock != nil {
			if err := lock.Release(); err != nil {
				fmt.Fprintf(os.Stderr, "helper: Release on iteration %d: %v\n", iteration, err)
				os.Exit(6)
			}
		}
	}
}

// TestAcquireBlocksWhileAnotherProcessHoldsTheLockAndProceedsWhenItIsReleased
// is the central claim of this package. It asserts both halves: that Acquire
// does not return while a separate process holds the lock, and that it returns
// promptly once that process releases it.
func TestAcquireBlocksWhileAnotherProcessHoldsTheLockAndProceedsWhenItIsReleased(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "session.json.lock")

	holder := startHelper(t, holdRole, map[string]string{helperLockPathEnv: path})
	holder.waitUntilReady(t)

	type attempt struct {
		lock    *Lock
		err     error
		elapsed time.Duration
	}
	attempts := make(chan attempt, 1)
	started := time.Now()
	go func() {
		lock, err := Acquire(testContext(t), path)
		attempts <- attempt{lock: lock, err: err, elapsed: time.Since(started)}
	}()

	// The negative half. If Acquire returns here, the lock does not exclude a
	// second process and everything else in this package is theatre.
	const holdWindow = 750 * time.Millisecond
	select {
	case result := <-attempts:
		if result.lock != nil {
			_ = result.lock.Release()
		}
		t.Fatalf("Acquire returned after %s while another process still held the lock (err=%v)\nholder output:\n%s",
			result.elapsed, result.err, holder.diagnostics())
	case <-time.After(holdWindow):
	}

	holder.release(t)
	holder.waitUntilExit(t)

	select {
	case result := <-attempts:
		if result.err != nil {
			t.Fatalf("Acquire failed after the holder released the lock: %v\nholder output:\n%s", result.err, holder.diagnostics())
		}
		if result.elapsed < holdWindow {
			t.Fatalf("Acquire took %s, want at least the %s the other process held the lock; "+
				"a shorter time means it never actually waited", result.elapsed, holdWindow)
		}
		// The wait is bounded by the poll backoff, so the hand-off must be
		// quick. A large overshoot would mean the backoff is not converging.
		if overshoot := result.elapsed - holdWindow; overshoot > 5*time.Second {
			t.Fatalf("Acquire took %s longer than the hold window, want the hand-off to be prompt", overshoot)
		}
		t.Logf("Acquire blocked for %s while another process held the lock, then took it %s after release",
			result.elapsed, result.elapsed-holdWindow)
		if err := result.lock.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
	case <-time.After(testBudget):
		t.Fatalf("Acquire never returned after the holder released the lock\nholder output:\n%s", holder.diagnostics())
	}
}

// TestConcurrentProcessesDoNotLoseUpdates is the property the metadata fix
// needs: N processes each performing a read-modify-write on one shared document
// must produce N increments, not fewer. The unlocked subtest is the control. It
// runs the identical helpers with the lock disabled and requires that updates
// ARE lost, which is what tells you the locked subtest is measuring the lock
// rather than a workload too gentle to race.
func TestConcurrentProcessesDoNotLoseUpdates(t *testing.T) {
	const (
		processes  = 8
		iterations = 20
	)

	t.Run("with the lock held around each read-modify-write", func(t *testing.T) {
		total := runCounterHelpers(t, processes, iterations, false)
		if total != processes*iterations {
			t.Fatalf("counter is %d after %d processes x %d increments, want %d: %d updates were lost, "+
				"so the lock did not serialise the read-modify-writes",
				total, processes, iterations, processes*iterations, processes*iterations-total)
		}
		t.Logf("%d processes x %d increments landed all %d updates", processes, iterations, total)
	})

	t.Run("without the lock, as a control", func(t *testing.T) {
		total := runCounterHelpers(t, processes, iterations, true)
		if total >= processes*iterations {
			t.Fatalf("the unlocked control reached %d of %d increments without losing one; the workload is not "+
				"actually racing, so the locked case above proves nothing", total, processes*iterations)
		}
		t.Logf("the unlocked control lost %d of %d updates (counter reached %d)",
			processes*iterations-total, processes*iterations, total)
	})
}

func runCounterHelpers(t *testing.T, processes, iterations int, withoutLock bool) int {
	t.Helper()
	directory := t.TempDir()
	lockPath := filepath.Join(directory, "counter.lock")
	counterPath := filepath.Join(directory, "counter")
	if err := writeCounter(counterPath, 0); err != nil {
		t.Fatal(err)
	}

	environment := map[string]string{
		helperLockPathEnv:   lockPath,
		helperCounterEnv:    counterPath,
		helperIterationsEnv: strconv.Itoa(iterations),
	}
	if withoutLock {
		environment[helperUnlockedEnv] = "1"
	}

	helpers := make([]*helperProcess, 0, processes)
	for index := 0; index < processes; index++ {
		helpers = append(helpers, startHelper(t, incrementRole, environment))
	}
	for _, helper := range helpers {
		helper.waitUntilReady(t)
	}
	for _, helper := range helpers {
		helper.release(t)
	}
	for _, helper := range helpers {
		helper.waitUntilExit(t)
	}

	total, err := readCounter(counterPath)
	if err != nil {
		t.Fatalf("read the shared counter: %v", err)
	}
	return total
}

// TestAcquireHonoursCancellationWhileAnotherProcessHoldsTheLock proves the
// second requirement: a caller that gives up gets its goroutine back. It also
// proves that giving up costs nothing durable, by repeating the abandoned
// acquisition many times and checking that neither goroutines nor file
// descriptors accumulate — the failure mode of the obvious implementation,
// where a blocking flock is parked in a goroutine that outlives the call.
func TestAcquireHonoursCancellationWhileAnotherProcessHoldsTheLock(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "session.json.lock")

	holder := startHelper(t, holdRole, map[string]string{helperLockPathEnv: path})
	holder.waitUntilReady(t)

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), contendedWait)
		defer cancel()
		started := time.Now()
		lock, err := Acquire(ctx, path)
		elapsed := time.Since(started)
		if err == nil {
			_ = lock.Release()
			t.Fatal("Acquire returned a lock another process is holding")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Acquire returned %v, want context.DeadlineExceeded", err)
		}
		if elapsed < contendedWait {
			t.Fatalf("Acquire gave up after %s, before its %s deadline", elapsed, contendedWait)
		}
		if elapsed > contendedWait+5*time.Second {
			t.Fatalf("Acquire returned %s after its deadline, want it to return promptly", elapsed-contendedWait)
		}
	})

	t.Run("explicit cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan error, 1)
		started := time.Now()
		go func() {
			lock, err := Acquire(ctx, path)
			if lock != nil {
				_ = lock.Release()
			}
			results <- err
		}()
		time.Sleep(contendedWait)
		cancel()
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Acquire returned %v, want context.Canceled", err)
			}
			t.Logf("Acquire returned %s after cancel", time.Since(started)-contendedWait)
		case <-time.After(testBudget):
			t.Fatal("Acquire did not return after its context was cancelled")
		}
	})

	t.Run("abandoned acquisitions leak nothing", func(t *testing.T) {
		const attempts = 30
		// Warm up so that one-off allocations from the first attempt are not
		// counted as growth.
		abandonAcquire(t, path)
		goroutinesBefore := runtime.NumGoroutine()
		descriptorsBefore, descriptorsCountable := openDescriptorCount()

		for attempt := 0; attempt < attempts; attempt++ {
			abandonAcquire(t, path)
		}

		goroutinesAfter := runtime.NumGoroutine()
		if goroutinesAfter > goroutinesBefore {
			t.Fatalf("%d abandoned acquisitions left %d extra goroutines running (%d -> %d); "+
				"a cancelled Acquire must not leave anything waiting on the lock",
				attempts, goroutinesAfter-goroutinesBefore, goroutinesBefore, goroutinesAfter)
		}
		if descriptorsCountable {
			descriptorsAfter, _ := openDescriptorCount()
			// Two descriptors of slack for the reads that do the counting.
			if descriptorsAfter > descriptorsBefore+2 {
				t.Fatalf("%d abandoned acquisitions left %d extra open descriptors (%d -> %d)",
					attempts, descriptorsAfter-descriptorsBefore, descriptorsBefore, descriptorsAfter)
			}
			t.Logf("%d abandoned acquisitions: goroutines %d -> %d, descriptors %d -> %d",
				attempts, goroutinesBefore, goroutinesAfter, descriptorsBefore, descriptorsAfter)
		} else {
			t.Logf("%d abandoned acquisitions: goroutines %d -> %d, descriptor count unavailable on this platform",
				attempts, goroutinesBefore, goroutinesAfter)
		}
	})

	// The holder must still be holding the lock through all of the above: if it
	// had exited early the cancellation assertions would have been measuring an
	// uncontended path.
	holder.assertStillRunning(t)
	holder.release(t)
	holder.waitUntilExit(t)
}

func abandonAcquire(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lock, err := Acquire(ctx, path)
	if err == nil {
		_ = lock.Release()
		t.Fatal("Acquire returned a lock another process is holding")
	}
}

// openDescriptorCount reports how many descriptors this process has open. It
// reports false where that cannot be observed, which keeps the leak assertion
// from turning into a portability problem.
//
// Readdirnames is used rather than os.ReadDir because os.ReadDir stats each
// entry, and on macOS statting the descriptor that is itself reading /dev/fd
// fails with EBADF, which would make every count unavailable. Only the names
// are wanted here.
func openDescriptorCount() (int, bool) {
	for _, directory := range []string{"/proc/self/fd", "/dev/fd"} {
		open, err := os.Open(directory)
		if err != nil {
			continue
		}
		names, err := open.Readdirnames(-1)
		_ = open.Close()
		if err != nil {
			continue
		}
		// The descriptor doing the counting is included, and it is included in
		// every measurement, so it cancels out between them.
		return len(names), true
	}
	return 0, false
}

type helperProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  *lockedBuffer
	exit    chan error
	ready   chan error
}

func startHelper(t *testing.T, role string, environment map[string]string) *helperProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		executable,
		"-test.run=^TestFileLockHelperProcess$",
		"-test.count=1",
		"-test.timeout="+helperBudget.String(),
	)
	command.Env = append(os.Environ(), helperRoleEnv+"="+role)
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The readiness pipe is created here rather than with Cmd.StdoutPipe
	// because Cmd.Wait closes a StdoutPipe as soon as the child exits, which
	// races with a concurrent reader. This end belongs to the test alone.
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = writeEnd
	stderr := &lockedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		t.Fatal(err)
	}
	// The child holds the only writer now; keeping the parent's copy open would
	// stop the reader from ever seeing end-of-file.
	_ = writeEnd.Close()

	helper := &helperProcess{
		command: command,
		stdin:   stdin,
		stdout:  readEnd,
		stderr:  stderr,
		exit:    make(chan error, 1),
		ready:   make(chan error, 1),
	}
	go func() { helper.exit <- command.Wait() }()
	go func() {
		announcement := make([]byte, len(helperReady))
		if _, err := io.ReadFull(readEnd, announcement); err != nil {
			helper.ready <- fmt.Errorf("read the readiness announcement: %w", err)
			return
		}
		if string(announcement) != helperReady {
			helper.ready <- fmt.Errorf("helper announced %q, want %q", announcement, helperReady)
			return
		}
		helper.ready <- nil
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		if command.ProcessState == nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		// Closing after the kill unblocks the readiness goroutine if the helper
		// died before announcing itself.
		_ = readEnd.Close()
	})
	return helper
}

func (h *helperProcess) waitUntilReady(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.ready:
		if err != nil {
			t.Fatalf("helper never became ready: %v\nhelper output:\n%s", err, h.diagnostics())
		}
	case <-time.After(testBudget):
		t.Fatalf("helper did not become ready within %s\nhelper output:\n%s", testBudget, h.diagnostics())
	}
}

func (h *helperProcess) release(t *testing.T) {
	t.Helper()
	if _, err := h.stdin.Write([]byte{helperStart}); err != nil {
		t.Fatalf("release the helper process: %v\nhelper output:\n%s", err, h.diagnostics())
	}
}

func (h *helperProcess) waitUntilExit(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.exit:
		if err != nil {
			t.Fatalf("helper process failed: %v\nhelper output:\n%s", err, h.diagnostics())
		}
	case <-time.After(testBudget):
		_ = h.command.Process.Kill()
		t.Fatalf("helper process did not exit within %s\nhelper output:\n%s", testBudget, h.diagnostics())
	}
}

// assertStillRunning fails if the helper has already exited, which would mean
// an assertion about contention was made against a lock nobody was holding.
func (h *helperProcess) assertStillRunning(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.exit:
		t.Fatalf("helper process exited early (%v) so the lock was not held for the assertions above\nhelper output:\n%s",
			err, h.diagnostics())
	default:
	}
}

func (h *helperProcess) diagnostics() string {
	return strings.TrimSpace(h.stderr.String())
}

// readCounter and writeCounter model the production read-modify-write. The
// write is atomic — a temp file and a rename, as state.WriteMetadata does — so
// no reader can ever observe a partial value and the only defect the counter
// can record is an update that was overwritten.
func readCounter(path string) (int, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(encoded)))
	if err != nil {
		return 0, fmt.Errorf("parse counter %q: %w", encoded, err)
	}
	return value, nil
}

func writeCounter(path string, value int) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.WriteString(temporary, strconv.Itoa(value)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

type lockedBuffer struct {
	mutex sync.Mutex
	bytes []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return string(b.bytes)
}
