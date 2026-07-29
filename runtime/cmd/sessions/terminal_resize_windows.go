//go:build windows

package main

import (
	"context"
	"time"
)

// Windows does not expose SIGWINCH. Polling is deliberately limited to an
// interactive attach and sends no traffic unless the dimensions changed in
// the caller's resize closure.
func watchTerminalResize(ctx context.Context, resize func()) func() {
	watchContext, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchContext.Done():
				return
			case <-ticker.C:
				resize()
			}
		}
	}()
	return cancel
}
