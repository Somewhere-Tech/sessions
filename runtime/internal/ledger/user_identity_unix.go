//go:build !windows

package ledger

import (
	"os"
	"strconv"
)

func platformLocalUserCreatorID() (string, error) {
	return "uid:" + strconv.Itoa(os.Getuid()), nil
}
