//go:build windows

package main

import "os"

func defaultRunnerCommand() string {
	if shell := os.Getenv("COMSPEC"); shell != "" {
		return shell
	}
	return "cmd.exe"
}
