//go:build !windows

package main

func defaultRunnerCommand() string {
	return "/bin/bash"
}
