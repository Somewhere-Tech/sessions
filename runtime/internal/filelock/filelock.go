// Package filelock provides a cross-process exclusive advisory lock backed by
// the operating system's own file locking, so that two Sessions processes that
// are not related by fork — the daemon and a runner that launchd started — can
// serialise a read-modify-write on the same document.
//
// Atomic replacement is not mutual exclusion. A temp-file-plus-rename write
// guarantees that a reader never sees a half-written document; it guarantees
// nothing about two processes that each read the document, each modify their
// own copy, and each rename their copy back. Whichever renames last wins and
// the other update is gone. This package supplies the missing half.
//
// The lock is advisory: it excludes only participants that also call Acquire.
// Nothing stops an unlocked writer from renaming over the document, so every
// process on the read-modify-write path has to take the lock for it to mean
// anything.
//
// Lock a sidecar path, not the document itself. On Unix a flock lives on the
// open file description, which is bound to an inode; a rename-based write
// replaces the inode, so processes that locked the document directly would
// end up holding locks on different inodes at different times and would not
// exclude each other at all. Use a stable companion path — "<id>.json.lock"
// beside "<id>.json" — which nothing ever renames over or deletes.
package filelock

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// initialRetryDelay and maxRetryDelay bound the acquisition poll. See
	// Acquire for why acquisition polls at all. The floor keeps an uncontended
	// hand-off fast — the common case is one holder finishing a few hundred
	// microseconds of JSON work — and the ceiling keeps a process that is
	// waiting behind a long holder from spinning.
	initialRetryDelay = 1 * time.Millisecond
	maxRetryDelay     = 25 * time.Millisecond
)

// Lock is a held exclusive advisory lock. It is released by Release and by
// nothing else: the holder's death releases it too, because the kernel drops
// the lock when the last descriptor referring to it is closed, which is the
// property that keeps a crashed daemon from wedging every session on the host.
type Lock struct {
	path string

	// mutex guards released so that Release is safe to call from more than one
	// goroutine and safe to call more than once, which is what makes the
	// "defer lock.Release()" plus an explicit early Release pattern correct.
	mutex    sync.Mutex
	released bool
	file     *os.File
}

// Acquire blocks until it holds an exclusive advisory lock for path, or until
// ctx is cancelled or its deadline expires. The lock file is created if it does
// not exist; its containing directory is not, because the caller owns the
// directory layout and silently creating one hides a misconfigured state
// directory.
//
// Acquire is NOT re-entrant. Every call opens a fresh descriptor, and both
// backends lock per descriptor rather than per process, so a second Acquire for
// the same path from the same process blocks exactly as another process would.
// That is deliberate: the daemon has many goroutines touching one session
// document and a lock that quietly let a second goroutine through would be
// worse than no lock. It also means a goroutine that calls Acquire while
// already holding the same lock will block until ctx ends, which is the reason
// ctx is a required argument rather than an option.
//
// Acquisition polls with a bounded backoff instead of issuing one blocking
// flock(2). A blocking flock cannot be interrupted by a Go context: the calling
// thread sits in the kernel until the lock is granted. Moving it to a goroutine
// does not fix that, it relocates it — the goroutine and its descriptor outlive
// the cancelled call for as long as the current holder cares to hold, and when
// it finally is granted the lock it takes a turn that no caller wants and has
// to hand it straight back. Polling keeps every wait inside a select on
// ctx.Done(), so a cancelled Acquire returns having left no goroutine, no
// descriptor, and no claim on the lock behind it. The cost is up to
// maxRetryDelay of extra latency on hand-off, which is far below the cost of
// the JSON read-modify-write this lock protects.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	file, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	lock := &Lock{path: path, file: file}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	delay := initialRetryDelay
	for {
		held, err := lock.tryLock()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire lock %s: %w", path, err)
		}
		if held {
			return lock, nil
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			// Closing the descriptor is the whole cleanup: no lock was taken,
			// so there is nothing to unlock and nothing left running.
			_ = file.Close()
			return nil, fmt.Errorf("acquire lock %s: %w", path, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, maxRetryDelay)
	}
}

// openLockFile opens the lock file for locking without disturbing whatever is
// already in it. It never truncates: the lock is the file's identity, not its
// contents, and a caller may well have put something human-readable inside.
//
// os.OpenFile is used rather than a raw syscall on purpose. On Unix it sets
// O_CLOEXEC, so a runner the daemon execs does not silently inherit the
// descriptor and with it a lock the daemon believes only it holds. On Windows
// it opens with FILE_SHARE_READ|FILE_SHARE_WRITE, without which a second
// process could not open the lock file at all and would fail long before it
// reached LockFileEx.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// Release drops the lock. It is safe to call more than once; calls after the
// first do nothing and report no error, so the error a caller must not ignore
// is the one from the first call.
//
// Release never removes the lock file, and no caller should either. Unlinking a
// lock file breaks the mutual exclusion it was there to provide, because the
// lock lives on the inode and the path is only a way to reach it: process A
// releases and unlinks, process B — which opened the path before the unlink and
// is waiting on the now-orphaned inode — is granted the lock on a file that no
// longer has a name, and process C then creates a fresh file at the same path
// and is granted a lock on that new inode immediately. B and C both believe
// they hold the lock for the same path and neither is wrong about its own
// inode. A leftover empty lock file costs one directory entry; leftover lost
// updates cost a session that will not resume.
func (l *Lock) Release() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	if err := l.unlock(); err != nil {
		return fmt.Errorf("release lock %s: %w", l.path, err)
	}
	return nil
}
