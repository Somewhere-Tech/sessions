package localnetwork

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

const (
	Reason  = "local-network-permission"
	Message = "macOS has not allowed Sessions to use the local network. System Settings › Privacy & Security › Local Network › turn on Sessions."
)

// Explain replaces Darwin's misleading EHOSTUNREACH text only when the failed
// destination is actually on a private or link-local network. Tailnet and
// public-network failures keep their original transport error.
func Explain(endpoint string, err error) error {
	if err == nil || !platformDenied(err) || !IsLocalEndpoint(endpoint) {
		return err
	}
	return errors.New(Message)
}

func IsPermissionError(err error) bool {
	return err != nil && err.Error() == Message
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
