//go:build !darwin

package discovery

import "net"

func platformAdvertise(address net.IP, port int, instance, hostLabel string, txt []string) (Registration, error) {
	return advertiseWithMDNS(address, port, instance, hostLabel, txt)
}
