package state

import (
	"errors"
	"fmt"
	"os"
)

// RecoverAfterBoot records stopped runtimes before discovery can retire their
// coordination files. Detached hosts have no per-runner boot supervisor: they
// preserve the launch record and require an explicit wake, never repeat a
// command or pretend its process survived an OS reboot.
func (l *DetachedLauncher) RecoverAfterBoot() error {
	bootID, err := CurrentBootID()
	if err != nil {
		return err
	}
	return markDetachedBootRecovery(l.config.RunnerStateDir, bootID)
}

func markDetachedBootRecovery(dir, bootID string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		id, ok := RunnerIDFromMetadataName(entry.Name())
		if !ok {
			continue
		}
		paths := For(dir, id)
		// A launch sidecar is proof this was a detached runtime. Never turn
		// another launcher's metadata into a different recovery workflow.
		if _, err := readLaunchSpec(paths.Launch); err != nil {
			continue
		}
		permit, err := readRestartPermit(paths.KeepAlive)
		if err != nil || permit.BootID == bootID {
			continue
		}
		reason := "paused after an OS reboot; the previous process ended; explicitly resume or wake this runtime to continue from retained history"
		if err := WriteRestorePending(paths.RestorePending, id, reason); err != nil {
			failures = append(failures, fmt.Errorf("record reboot recovery for %s: %w", id, err))
			continue
		}
		// Keep the old permit until an explicit wake renews it. This makes
		// startup retry safe when writing a marker was interrupted.
	}
	return errors.Join(failures...)
}
