//go:build linux

package watch

// processParents returns a pid -> ppid snapshot of the process table, read
// from /proc. See proc.go for why this is not a `ps` call on Linux.
//
// An empty result means "unknown ancestry", which callers resolve to
// not-owned -- and not-owned is the conservative answer, because it makes
// Sessions treat the process as external and refuse to touch its conversation.
func processParents() map[int]int {
	return procParents("/proc")
}
