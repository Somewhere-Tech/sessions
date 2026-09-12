//go:build !darwin && !linux

package state

import "time"

// readBootTime is unavailable here. A caller that cannot learn when the
// machine started says "daemon lost contact" rather than guessing a reboot.
func readBootTime() (time.Time, bool) { return time.Time{}, false }
