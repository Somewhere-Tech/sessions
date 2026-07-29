package lan

import (
	"errors"
	"fmt"
	"net"
)

type networkInterface struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

func PrimaryIPv4() (net.IP, error) {
	interfaces, err := systemInterfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	routedIP, err := defaultRouteIPv4()
	if err != nil {
		return nil, err
	}
	return pickPrimaryIPv4(interfaces, routedIP)
}

func systemInterfaces() ([]networkInterface, error) {
	system, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	interfaces := make([]networkInterface, 0, len(system))
	for _, item := range system {
		addresses, err := item.Addrs()
		if err != nil {
			continue
		}
		interfaces = append(interfaces, networkInterface{name: item.Name, flags: item.Flags, addrs: addresses})
	}
	return interfaces, nil
}

func defaultRouteIPv4() (net.IP, error) {
	connection, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(1, 1, 1, 1), Port: 53})
	if err != nil {
		return nil, fmt.Errorf("find the default IPv4 route: %w", err)
	}
	defer connection.Close()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP == nil {
		return nil, errors.New("find the default IPv4 route: no source address")
	}
	return address.IP.To4(), nil
}

func pickPrimaryIPv4(interfaces []networkInterface, routedIP net.IP) (net.IP, error) {
	routedIP = routedIP.To4()
	if routedIP == nil {
		return nil, noLANInterfaceError()
	}
	for _, item := range interfaces {
		if item.flags&net.FlagUp == 0 || item.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, address := range item.addrs {
			ip := addressIP(address)
			if ip == nil || !ip.Equal(routedIP) || !eligibleLANIPv4(ip) {
				continue
			}
			return append(net.IP(nil), ip.To4()...), nil
		}
	}
	return nil, noLANInterfaceError()
}

func addressIP(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP.To4()
	case *net.IPAddr:
		return value.IP.To4()
	default:
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil {
			return nil
		}
		return ip.To4()
	}
}

func eligibleLANIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsPrivate() {
		return false
	}
	// Tailscale IPv4 addresses occupy 100.64.0.0/10. They are not RFC 1918,
	// but keep the explicit exclusion here as part of the LAN-mode contract.
	if ip[0] == 100 && ip[1]&0xc0 == 0x40 {
		return false
	}
	return true
}

func noLANInterfaceError() error {
	return errors.New("no active private LAN IPv4 was found on the default route; connect this Mac to Wi-Fi or Ethernet, then retry `sessions lan enable`")
}
