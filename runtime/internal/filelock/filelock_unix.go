//go:build unix

package filelock

import (
	"errors"

	"golang.org/x/sys/unix"
)

// tryLock attempts one non-blocking flock(2) and reports whether the lock is
// now held. LOCK_EX is the exclusive mode and LOCK_NB is what makes the attempt
// return instead of parking the calling thread in the kernel where no context
// could reach it.
//
// The flock is taken on the open file description, so it is released when the
// last descriptor referring to that description is closed — including by the
// kernel when a process dies, deliberately or otherwise. That is why a crash
// while holding this lock does not leave a permanently locked session behind,
// and it is also why the lock is bound to the inode rather than to the path.
func (l *Lock) tryLock() (bool, error) {
	connection, err := l.file.SyscallConn()
	if err != nil {
		return false, err
	}
	var lockErr error
	// Control keeps the descriptor alive for the duration of the call, which a
	// bare File.Fd() does not, and is the sanctioned way to hand a Go file's
	// descriptor to a syscall.
	if err := connection.Control(func(descriptor uintptr) {
		lockErr = unix.Flock(int(descriptor), unix.LOCK_EX|unix.LOCK_NB)
	}); err != nil {
		return false, err
	}
	switch {
	case lockErr == nil:
		return true, nil
	case errors.Is(lockErr, unix.EWOULDBLOCK):
		// Another descriptor holds it. EWOULDBLOCK and EAGAIN are the same
		// value on every platform this builds for; errors.Is covers both names.
		return false, nil
	case errors.Is(lockErr, unix.EINTR):
		// A signal arrived before the kernel could answer. Nothing is known
		// about the lock, so treat it as contention and ask again.
		return false, nil
	default:
		return false, lockErr
	}
}

// unlock drops the flock explicitly before closing. Closing alone would release
// it, but doing so implicitly would mean a close error and a release error were
// indistinguishable to the caller.
func (l *Lock) unlock() error {
	connection, err := l.file.SyscallConn()
	if err != nil {
		_ = l.file.Close()
		return err
	}
	var unlockErr error
	if err := connection.Control(func(descriptor uintptr) {
		unlockErr = unix.Flock(int(descriptor), unix.LOCK_UN)
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
