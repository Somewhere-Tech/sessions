//go:build !windows

package codexapp

func useDirectAppServerFallback() bool { return false }
