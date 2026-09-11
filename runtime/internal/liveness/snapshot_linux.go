//go:build linux

package liveness

import "context"

// ProcessSnapshot reads every PID and command with one /proc walk. Discovery
// and listing use it instead of launching one probe per stale session, and on
// Linux it needs no external binary: a container without ps still answers.
func ProcessSnapshot(context.Context) (map[int]string, error) {
	return procSnapshot("/proc")
}
