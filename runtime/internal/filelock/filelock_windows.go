//go:build windows

package filelock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// allBytes is the byte count locked by every call here. Windows byte-range
// locks are ranges, not whole-file locks, and two processes that locked
// different ranges of the same file would not exclude each other at all, so
// every participant must agree on one range. The whole 64-bit range starting at
// offset zero is that agreement, and it is the same range Go's own module cache
// locking uses.
const allBytes = ^uint32(0)

// tryLock attempts one non-blocking LockFileEx and reports whether the lock is
// now held. LOCKFILE_EXCLUSIVE_LOCK is the exclusive mode; LOCKFILE_FAIL_IMMEDIATELY
// is what makes the attempt return ERROR_LOCK_VIOLATION instead of queueing the
// request in the kernel where no context could reach it.
//
// The lock is held by the file object behind this handle, so Windows releases
// it when the handle closes, including when the kernel closes it on process
// exit. A crash therefore does not strand the lock, which matches the Unix
// backend's behaviour closely enough that callers need not distinguish them.
func (l *Lock) tryLock() (bool, error) {
	connection, err := l.file.SyscallConn()
	if err != nil {
		return false, err
	}
	var lockErr error
	if err := connection.Control(func(handle uintptr) {
		var overlapped windows.Overlapped
		lockErr = windows.LockFileEx(
			windows.Handle(handle),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			allBytes,
			allBytes,
			&overlapped,
		)
	}); err != nil {
		return false, err
	}
	switch {
	case lockErr == nil:
		return true, nil
	case errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION):
		// Another handle holds the range. This is the only outcome that means
		// "wait"; anything else is reported so a real failure is never mistaken
		// for contention and retried forever.
		return false, nil
	default:
		return false, lockErr
	}
}

// unlock releases the byte range explicitly before closing the handle. The
// range and the zero offset must match the ones tryLock used, which is why the
// Overlapped is freshly zeroed here as it was there.
func (l *Lock) unlock() error {
	connection, err := l.file.SyscallConn()
	if err != nil {
		_ = l.file.Close()
		return err
	}
	var unlockErr error
	if err := connection.Control(func(handle uintptr) {
		var overlapped windows.Overlapped
		unlockErr = windows.UnlockFileEx(windows.Handle(handle), 0, allBytes, allBytes, &overlapped)
	}); err != nil {
		_ = l.file.Close()
		return err
	}
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
