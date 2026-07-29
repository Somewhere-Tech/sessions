//go:build !windows

package main

import "os"

func watchPlatformStop(chan<- os.Signal) func() {
	return func() {}
}
