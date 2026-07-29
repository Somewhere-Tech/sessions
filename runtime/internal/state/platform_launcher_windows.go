//go:build windows

package state

import "github.com/somewhere-tech/sessions/runtime/internal/proto"

func NewPlatformLauncher(config Config) proto.RunnerLauncher {
	return NewWindowsLauncher(config)
}
