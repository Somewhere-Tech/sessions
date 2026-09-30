//go:build darwin

package state

import (
	"os/exec"
	"strings"
)

func platformComputerName() string {
	for _, key := range []string{"ComputerName", "LocalHostName"} {
		output, err := exec.Command("/usr/sbin/scutil", "--get", key).Output()
		if err == nil {
			if name := strings.TrimSpace(string(output)); name != "" {
				return name
			}
		}
	}
	return ""
}
