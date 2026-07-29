//go:build windows

package codexapp

// Codex's portable structured transport is stdio. The Unix-socket fallback is
// intentionally unavailable on Windows.
func useDirectAppServerFallback() bool { return true }
