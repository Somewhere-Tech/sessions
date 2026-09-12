package state

import (
	"sync"
	"time"
)

// BootTime is when this machine last started, read once.
//
// The reading is platform work — a sysctl subprocess on macOS — and it is on
// the path of every session listing, which runs several times a second while
// the app is open. It cannot change while this process lives: a reboot takes
// the daemon with it.
func BootTime() (time.Time, bool) {
	bootOnce.Do(func() { bootAt, bootKnown = readBootTime() })
	return bootAt, bootKnown
}

var (
	bootOnce  sync.Once
	bootAt    time.Time
	bootKnown bool
)
