package lan

import (
	"net"
	"testing"
)

func TestPickPrimaryIPv4SkipsIneligibleAddresses(t *testing.T) {
	interfaces := []networkInterface{
		{name: "lo0", flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{cidr(t, "127.0.0.1/8")}},
		{name: "en0", flags: net.FlagUp, addrs: []net.Addr{
			cidr(t, "169.254.10.20/16"),
			cidr(t, "100.96.4.2/10"),
			cidr(t, "192.168.4.25/24"),
		}},
		{name: "en1", flags: 0, addrs: []net.Addr{cidr(t, "10.0.0.9/24")}},
	}
	got, err := pickPrimaryIPv4(interfaces, net.ParseIP("192.168.4.25"))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.168.4.25" {
		t.Fatalf("picked %s, want 192.168.4.25", got)
	}
	for _, routed := range []string{"127.0.0.1", "169.254.10.20", "100.96.4.2", "203.0.113.8"} {
		if got, err := pickPrimaryIPv4(interfaces, net.ParseIP(routed)); err == nil {
			t.Fatalf("routed IP %s unexpectedly selected %s", routed, got)
		}
	}
}

func cidr(t *testing.T, value string) net.Addr {
	t.Helper()
	ip, network, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatal(err)
	}
	network.IP = ip
	return network
}
