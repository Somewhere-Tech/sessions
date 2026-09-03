package state

import (
	"os"
	"strings"
)

const fallbackComputerName = "Sessions machine"

// ComputerName returns the name a person sees for this computer in the host
// operating system. Platform adapters prefer that display name; the DNS
// hostname remains the portable fallback.
func ComputerName() string {
	for _, candidate := range []string{platformComputerName(), hostname()} {
		if name := normalizeComputerName(candidate); name != "" {
			return name
		}
	}
	return fallbackComputerName
}

func hostname() string {
	name, _ := os.Hostname()
	return name
}

func normalizeComputerName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len(".local") && strings.EqualFold(value[len(value)-len(".local"):], ".local") {
		value = strings.TrimSpace(value[:len(value)-len(".local")])
	}
	return value
}
