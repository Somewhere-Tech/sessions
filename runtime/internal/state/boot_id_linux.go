//go:build linux

package state

import (
	"fmt"
	"os"
	"strings"
)

func CurrentBootID() (string, error) {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	if trimmed := strings.TrimSpace(string(value)); trimmed != "" {
		return "linux-" + trimmed, nil
	}
	return "", fmt.Errorf("read boot id: empty value")
}
