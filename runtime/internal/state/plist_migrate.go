package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const legacyRestartPolicy = `<key>RunAtLoad</key>
  <true/>
  <!-- Restart the runner if it dies UNEXPECTEDLY (crash, kill -9,
       sessionsd-side socket cleanup nudging it out), but NOT when the
       underlying PTY closes normally. -->
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>`

const environmentDictionaryStart = `<key>EnvironmentVariables</key>
  <dict>`

const bootScopedRestartEnvironment = `<key>EnvironmentVariables</key>
  <dict>
    <key>RUNNER_RESTART_POLICY</key>
    <string>boot-scoped</string>`

// MigrateRunnerPlistRestartPolicies makes already-installed runner jobs safe
// for the next login. It changes only the exact policy emitted by Sessions;
// unknown or hand-edited launch agents are left untouched.
func MigrateRunnerPlistRestartPolicies(launchAgentsDir, runnerStateDir string) (int, error) {
	entries, err := os.ReadDir(launchAgentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	migrated := 0
	var migrationErrors []error
	bootID, bootErr := CurrentBootID()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, launchdLabelPrefix) || !strings.HasSuffix(name, ".plist") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, launchdLabelPrefix), ".plist")
		if id == "" {
			continue
		}
		path := filepath.Join(launchAgentsDir, name)
		encoded, readErr := os.ReadFile(path)
		if readErr != nil {
			migrationErrors = append(migrationErrors, readErr)
			continue
		}
		if !strings.Contains(string(encoded), legacyRestartPolicy) {
			continue
		}
		paths := For(runnerStateDir, id)
		keepAlivePath := paths.KeepAlive
		if bootErr != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("migrate %s: identify current boot: %w", name, bootErr))
			continue
		}
		// The old job was eligible to run at login. Seed a current-boot permit
		// before replacing that policy so its next launch can distinguish a
		// same-boot crash from a new boot. On the next boot, every runner gets
		// only far enough to apply the bounded pinned-root policy; providers do
		// not start unless that policy admits them.
		if writeErr := WriteRestartPermit(keepAlivePath, bootID); writeErr != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("migrate %s permit: %w", name, writeErr))
			continue
		}
		replacement := `<key>RunAtLoad</key>
  <false/>
  <!-- Same-boot crashes restart through a boot-scoped PathState permit.
       No permit means this retained session stays stopped after login. -->
  <key>KeepAlive</key>
  <dict>
    <key>PathState</key>
    <dict>
      <key>` + xmlEscape(keepAlivePath) + `</key>
      <true/>
    </dict>
  </dict>`
		updated := strings.Replace(string(encoded), legacyRestartPolicy, replacement, 1)
		if !strings.Contains(updated, "<key>RUNNER_RESTART_POLICY</key>") {
			if !strings.Contains(updated, environmentDictionaryStart) {
				_ = os.Remove(keepAlivePath)
				migrationErrors = append(migrationErrors, fmt.Errorf("migrate %s: canonical environment dictionary not found", name))
				continue
			}
			updated = strings.Replace(updated, environmentDictionaryStart, bootScopedRestartEnvironment, 1)
		}
		if writeErr := writeProtectedFile(path, []byte(updated)); writeErr != nil {
			_ = os.Remove(keepAlivePath)
			migrationErrors = append(migrationErrors, fmt.Errorf("migrate %s: %w", name, writeErr))
			continue
		}
		migrated++
	}
	return migrated, errors.Join(migrationErrors...)
}

func xmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

func writeProtectedFile(path string, value []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".sessions-runner-plist-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
