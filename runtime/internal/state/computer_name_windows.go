//go:build windows

package state

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func platformComputerName() string {
	if name := strings.TrimSpace(os.Getenv("COMPUTERNAME")); name != "" {
		return name
	}
	for _, path := range []string{
		`SYSTEM\CurrentControlSet\Control\ComputerName\ActiveComputerName`,
		`SYSTEM\CurrentControlSet\Control\ComputerName\ComputerName`,
	} {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		name, _, readErr := key.GetStringValue("ComputerName")
		key.Close()
		if readErr == nil && strings.TrimSpace(name) != "" {
			return name
		}
	}
	return ""
}
