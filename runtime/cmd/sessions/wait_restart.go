package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

const waitRestartNotice = "sessionsd restarted; still waiting"

type waitRestart struct {
	mu        sync.Mutex
	app       *app
	announced bool
	failures  int
}

func (retry *waitRestart) pause(err error, deadline time.Time) bool {
	if !isRestartTransportError(err) || !retry.app.now().Before(deadline) {
		return false
	}
	retry.mu.Lock()
	defer retry.mu.Unlock()
	if !retry.announced {
		_, _ = fmt.Fprintln(retry.app.stderr, waitRestartNotice)
		retry.announced = true
	}
	delay := 100 * time.Millisecond * time.Duration(1<<min(retry.failures, 4))
	retry.failures++
	remaining := deadline.Sub(retry.app.now())
	if delay > remaining {
		delay = remaining
	}
	if delay > 0 {
		retry.app.sleep(delay)
	}
	return true
}

func (retry *waitRestart) reset() {
	retry.mu.Lock()
	retry.failures = 0
	retry.mu.Unlock()
}

func isRestartTransportError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
