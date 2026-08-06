package filelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// contendedWait is how long a test waits before concluding that an Acquire
// which should be blocked really is blocked. It is also the deadline given to
// an Acquire that is expected to time out. It is far larger than maxRetryDelay,
// so a passing result is not an artefact of the poll interval.
const contendedWait = 300 * time.Millisecond

func TestAcquireAndReleaseOnAnUncontendedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json.lock")
	lock, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire on an uncontended path: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// The same path must be immediately usable again, otherwise release did
	// not actually release.
	second, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release the second lock: %v", err)
	}
}

// TestReleaseIsIdempotentAndKeepsTheSameLockFile pins the rule documented on
// Release: releasing twice is harmless, and releasing never unlinks the file.
// The identity check is the part that matters. If Release removed the file, a
// later Acquire would create a different inode at the same path, and two
// processes could hold locks on two inodes while believing they had excluded
// each other.
func TestReleaseIsIdempotentAndKeepsTheSameLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json.lock")
	lock, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	beforeRelease, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the lock file while held: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release must be a no-op, got: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("third Release must be a no-op, got: %v", err)
	}

	afterRelease, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the lock file must survive Release, stat failed: %v", err)
	}
	if !os.SameFile(beforeRelease, afterRelease) {
		t.Fatal("Release replaced the lock file with a different inode; two processes " +
			"reaching this path could then lock different inodes and both believe they hold it")
	}

	// And a fresh Acquire must land on that same inode rather than create one.
	reacquired, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	defer reacquired.Release()
	afterReacquire, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after re-Acquire: %v", err)
	}
	if !os.SameFile(beforeRelease, afterReacquire) {
		t.Fatal("re-Acquire created a new inode at the lock path instead of reusing the existing file")
	}
}

// TestAcquireIsNotReentrantWithinOneProcess documents and pins the design
// choice. Both backends lock per open file description, not per process, and
// Acquire opens a new one every call, so a second Acquire for a path this
// process already holds blocks exactly as a second process would. A lock that
// silently admitted a second goroutine of the same daemon would not protect the
// document it exists to protect.
func TestAcquireIsNotReentrantWithinOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json.lock")
	held, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), contendedWait)
	defer cancel()
	started := time.Now()
	second, err := Acquire(ctx, path)
	elapsed := time.Since(started)
	if err == nil {
		_ = second.Release()
		_ = held.Release()
		t.Fatal("Acquire is documented as non-re-entrant but granted a second lock for a path this process already holds")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = held.Release()
		t.Fatalf("re-entrant Acquire returned %v, want a context deadline error", err)
	}
	if elapsed < contendedWait {
		_ = held.Release()
		t.Fatalf("re-entrant Acquire gave up after %s, want it to have blocked for the full %s deadline", elapsed, contendedWait)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Once the first lock is gone the same call succeeds, which shows the
	// blocking above was the lock and not a broken path.
	third, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire after the first lock was released: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireReturnsWithoutBlockingWhenContextIsAlreadyDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json.lock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	lock, err := Acquire(ctx, path)
	elapsed := time.Since(started)
	if err == nil {
		_ = lock.Release()
		t.Fatal("Acquire with an already-cancelled context returned a lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire returned %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Acquire with a cancelled context took %s, want an immediate return", elapsed)
	}
	// Nothing was created either: a caller that cancelled before it started
	// should not leave a lock file behind in a directory it may not own.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Acquire created %s despite an already-cancelled context (stat: %v)", path, err)
	}
}

func TestAcquireFailsWhenTheDirectoryDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "session.json.lock")
	started := time.Now()
	lock, err := Acquire(testContext(t), path)
	if err == nil {
		_ = lock.Release()
		t.Fatal("Acquire succeeded with a missing parent directory")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Acquire returned %v, want a not-exist error", err)
	}
	// A missing directory is permanent, so it must be reported rather than
	// retried until the context expires.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Acquire spent %s on a missing directory, want an immediate failure", elapsed)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("Acquire error %q does not name the path, which is all a log line has to go on", err)
	}
}

func TestAcquireFailsWhenTheLockFileCannotBeCreated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not deny creation on Windows the way this test assumes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode bits this test relies on")
	}
	readOnlyDir := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(readOnlyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(readOnlyDir, "session.json.lock")

	started := time.Now()
	lock, err := Acquire(testContext(t), path)
	if err == nil {
		_ = lock.Release()
		t.Fatal("Acquire succeeded in a directory it cannot write to")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Acquire returned %v, want a permission error", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Acquire spent %s on an unwritable directory, want an immediate failure", elapsed)
	}
}

// TestAcquireDoesNotTruncateTheLockFile matters because a caller may put a
// human-readable note in the lock file, and because truncation on open would be
// a write to a file another process is concurrently locking.
func TestAcquireDoesNotTruncateTheLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json.lock")
	const note = "held by the sessions daemon\n"
	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(testContext(t), path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != note {
		t.Fatalf("lock file contents are %q after a lock cycle, want %q", contents, note)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testBudget)
	t.Cleanup(cancel)
	return ctx
}
