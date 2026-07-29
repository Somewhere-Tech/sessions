//go:build windows

package ledger

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func platformLocalUserCreatorID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read current process token user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return "", fmt.Errorf("current process token did not contain a user SID")
	}
	return "sid:" + user.User.Sid.String(), nil
}
