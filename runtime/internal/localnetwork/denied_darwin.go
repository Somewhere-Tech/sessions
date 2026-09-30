//go:build darwin

package localnetwork

import (
	"errors"
	"syscall"
)

func platformDenied(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH)
}
