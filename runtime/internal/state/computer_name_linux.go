//go:build linux

package state

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func platformComputerName() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "hostnamectl", "--pretty").Output(); err == nil {
		if name := strings.TrimSpace(string(output)); name != "" {
			return name
		}
	}
	encoded, err := os.ReadFile("/etc/machine-info")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "PRETTY_HOSTNAME" {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, unquoteErr := strconv.Unquote(value); unquoteErr == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, "'")
		}
		return strings.TrimSpace(value)
	}
	return ""
}
