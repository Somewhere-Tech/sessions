//go:build !windows

package state

func platformRunnerPath(value string) string {
	return launchdPath(value)
}
