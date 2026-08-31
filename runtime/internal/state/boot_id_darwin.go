//go:build darwin

package state

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var darwinBootSeconds = regexp.MustCompile(`sec\s*=\s*([0-9]+)`) // kern.boottime

// CurrentBootID identifies one macOS boot without requiring network or user
// state. launchd restart permits use it to distinguish a same-boot crash from
// a login after reboot.
func CurrentBootID() (string, error) {
	output, err := exec.Command("/usr/sbin/sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return "", fmt.Errorf("read kern.boottime: %w", err)
	}
	match := darwinBootSeconds.FindStringSubmatch(string(output))
	if len(match) != 2 {
		return "", fmt.Errorf("parse kern.boottime %q", strings.TrimSpace(string(output)))
	}
	return "darwin-" + match[1], nil
}
