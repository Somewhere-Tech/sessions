//go:build !windows

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"
)

func RunnerEndpoint(stateDirectory, id string) string {
	return filepath.Join(stateDirectory, id+".sock")
}

func Listen(endpoint string) (net.Listener, error) {
	return net.Listen("unix", endpoint)
}

func DialContext(ctx context.Context, endpoint string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
}

func DialTimeout(endpoint string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", endpoint, timeout)
}

func Remove(endpoint string) error {
	return os.Remove(endpoint)
}

func MayExist(endpoint string) bool {
	_, err := os.Stat(endpoint)
	return err == nil
}

func HasFilesystemMarker() bool {
	return true
}
