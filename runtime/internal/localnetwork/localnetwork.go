package localnetwork

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const (
	Reason = "local-network-permission"
	// PossibleCause is deliberately conditional. macOS offers no supported way
	// to read the Local Network switch, and a peer that is off, asleep, or on
	// another subnet fails a private-address dial with the same EHOSTUNREACH.
	// Claiming a denial from that errno told users to fix a setting that was
	// often already correct.
	PossibleCause = "macOS may not have allowed Sessions to use the local network — check System Settings › Privacy & Security › Local Network › Sessions; a machine that is off, asleep, or on another network fails the same way"
)

var errPossiblePermission = errors.New(PossibleCause)

// Explain annotates Darwin's terse EHOSTUNREACH with the permission as a
// possible cause when the failed destination is on a private or link-local
// network. The original error is preserved and reported first, so callers keep
// both the transport detail and errno matching. Tailnet and public-network
// failures are returned unchanged.
func Explain(endpoint string, err error) error {
	if err == nil || !platformDenied(err) || !IsLocalEndpoint(endpoint) {
		return err
	}
	return fmt.Errorf("%w (%w)", err, errPossiblePermission)
}

// IsPossiblePermissionError reports whether Explain added the local-network
// permission as one candidate cause. It is not evidence that macOS refused.
func IsPossiblePermissionError(err error) bool {
	return errors.Is(err, errPossiblePermission)
}

func IsLocalEndpoint(endpoint string) bool {
	host := strings.TrimSpace(endpoint)
	if parsed, err := url.Parse(host); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	} else if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast())
}
