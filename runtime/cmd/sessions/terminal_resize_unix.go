//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func watchTerminalResize(ctx context.Context, resize func()) func() {
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resizeSignals:
				resize()
			}
		}
	}()
	return func() {
		signal.Stop(resizeSignals)
	}
}
