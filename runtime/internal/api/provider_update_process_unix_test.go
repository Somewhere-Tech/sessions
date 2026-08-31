//go:build !windows

package api

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestProviderUpdateCancellationStopsDescendantHoldingOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & wait")
	configureProviderUpdateCommand(command)
	done := make(chan error, 1)
	go func() {
		_, err := command.CombinedOutput()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("provider update process tree remained alive after cancellation")
	}
}
