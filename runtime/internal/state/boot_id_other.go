//go:build !darwin && !linux

package state

import "errors"

func CurrentBootID() (string, error) {
	return "", errors.New("boot identity is unavailable on this platform")
}
