package discovery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

const (
	ServiceType = "_sessions._tcp"
	Domain      = "local."
)

// Candidate is intentionally low-sensitivity. A Bonjour announcement is only
// a hint that a Sessions-shaped service may exist; callers must still verify
// /api/health and complete the explicit access-approval flow.
type Candidate struct {
	Name      string `json:"name"`
	Hostname  string `json:"hostname"`
	Endpoint  string `json:"endpoint"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
}

type Registration interface {
	Shutdown() error
}

type AdvertiseFunc func(net.IP, int, string, string) (Registration, error)

func Advertise(address net.IP, port int, machineName, machineID string) (Registration, error) {
	ip := address.To4()
	if ip == nil || !privateIPv4(ip) {
		return nil, fmt.Errorf("Bonjour requires a private LAN IPv4 address")
	}
	if port < 1024 || port > 65535 {
		return nil, fmt.Errorf("Bonjour service port must be between 1024 and 65535")
	}
	instance := serviceInstance(machineName, machineID)
	hostLabel := "sessions-" + machineIDSuffix(machineID)
	return platformAdvertise(ip, port, instance, hostLabel)
}

func advertiseWithMDNS(address net.IP, port int, instance, hostLabel string) (Registration, error) {
	iface, err := interfaceForIP(address)
	if err != nil {
		return nil, err
	}
	service, err := mdns.NewMDNSService(
		instance,
		ServiceType,
		Domain,
		hostLabel+".local.",
		port,
		[]net.IP{address},
		[]string{"sessions=1", "api=1", "approval=required", "transport=http"},
	)
	if err != nil {
		return nil, fmt.Errorf("prepare Bonjour service: %w", err)
	}
	server, err := mdns.NewServer(&mdns.Config{
		Zone: service, Iface: iface, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		return nil, fmt.Errorf("start Bonjour service on %s: %w", iface.Name, err)
	}
	return server, nil
}

func Browse(ctx context.Context, timeout time.Duration) ([]Candidate, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	entries := make(chan *mdns.ServiceEntry, 64)
	params := mdns.DefaultParams(ServiceType)
	params.Timeout = timeout
	params.Entries = entries
	params.DisableIPv6 = true
	params.Logger = log.New(io.Discard, "", 0)
	collected := make(chan []*mdns.ServiceEntry, 1)
	go func() {
		found := make([]*mdns.ServiceEntry, 0, 16)
		for entry := range entries {
			found = append(found, entry)
		}
		collected <- found
	}()
	if err := mdns.QueryContext(ctx, params); err != nil {
		close(entries)
		<-collected
		return nil, fmt.Errorf("browse Bonjour services: %w", err)
	}
	close(entries)
	found := <-collected

	local := localIPv4s()
	byEndpoint := make(map[string]Candidate)
	for _, entry := range found {
		candidate, ok := candidateFromEntry(entry)
		if !ok || local[candidate.Address] {
			continue
		}
		byEndpoint[candidate.Endpoint] = candidate
	}
	candidates := make([]Candidate, 0, len(byEndpoint))
	for _, candidate := range byEndpoint {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if strings.EqualFold(candidates[i].Name, candidates[j].Name) {
			return candidates[i].Endpoint < candidates[j].Endpoint
		}
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[j].Name)
	})
	return candidates, nil
}

func candidateFromEntry(entry *mdns.ServiceEntry) (Candidate, bool) {
	if entry == nil || entry.Port < 1024 || entry.Port > 65535 {
		return Candidate{}, false
	}
	if !hasTXT(entry.InfoFields, "sessions", "1") ||
		!hasTXT(entry.InfoFields, "approval", "required") {
		return Candidate{}, false
	}
	ip := entry.AddrV4.To4()
	if ip == nil || !privateIPv4(ip) {
		return Candidate{}, false
	}
	address := ip.String()
	endpoint := "http://" + net.JoinHostPort(address, strconv.Itoa(entry.Port))
	name := strings.TrimSuffix(entry.Name, "."+ServiceType+".local.")
	name = strings.TrimSpace(strings.TrimSuffix(name, "."))
	name = unescapeDNSPresentation(name)
	if name == "" {
		name = "Sessions machine"
	}
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}
	hostname := strings.TrimSuffix(strings.TrimSpace(entry.Host), ".")
	return Candidate{
		Name: name, Hostname: hostname, Endpoint: endpoint,
		Address: address, Port: entry.Port, Transport: "nearby",
	}, true
}

func hasTXT(fields []string, key, value string) bool {
	for _, field := range fields {
		left, right, found := strings.Cut(field, "=")
		if found && strings.EqualFold(strings.TrimSpace(left), key) &&
			strings.EqualFold(strings.TrimSpace(right), value) {
			return true
		}
	}
	return false
}

func serviceInstance(machineName, machineID string) string {
	name := sanitizeServiceName(machineName)
	if name == "" {
		name = "Sessions machine"
	}
	suffix := " · " + machineIDSuffix(machineID)
	for len([]byte(name+suffix)) > 63 && len(name) > 0 {
		runes := []rune(name)
		name = strings.TrimSpace(string(runes[:len(runes)-1]))
	}
	if name == "" {
		name = "Sessions"
	}
	return name + suffix
}

func sanitizeServiceName(value string) string {
	value = strings.TrimSpace(value)
	var cleaned strings.Builder
	separator := false
	previousSpace := false
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character == '.' || character == '\\' {
			separator = cleaned.Len() > 0
			continue
		}
		currentSpace := character == ' '
		if separator && !previousSpace && !currentSpace {
			cleaned.WriteByte(' ')
		}
		cleaned.WriteRune(character)
		separator = false
		previousSpace = currentSpace
	}
	return strings.TrimSpace(cleaned.String())
}

func unescapeDNSPresentation(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			result.WriteByte(value[index])
			continue
		}
		if index+3 < len(value) &&
			value[index+1] >= '0' && value[index+1] <= '9' &&
			value[index+2] >= '0' && value[index+2] <= '9' &&
			value[index+3] >= '0' && value[index+3] <= '9' {
			decoded := int(value[index+1]-'0')*100 +
				int(value[index+2]-'0')*10 +
				int(value[index+3]-'0')
			if decoded <= 255 {
				result.WriteByte(byte(decoded))
				index += 3
				continue
			}
		}
		index++
		result.WriteByte(value[index])
	}
	return result.String()
}

func machineIDSuffix(machineID string) string {
	cleaned := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(machineID), "-", ""))
	if cleaned != "" {
		digest := sha256.Sum256([]byte("sessions-bonjour:" + cleaned))
		return fmt.Sprintf("%x", digest[:3])
	}
	return "nearby"
}

func interfaceForIP(ip net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces for Bonjour: %w", err)
	}
	for index := range interfaces {
		addresses, addressErr := interfaces[index].Addrs()
		if addressErr != nil {
			continue
		}
		for _, address := range addresses {
			var candidate net.IP
			switch value := address.(type) {
			case *net.IPNet:
				candidate = value.IP
			case *net.IPAddr:
				candidate = value.IP
			}
			if candidate != nil && candidate.Equal(ip) {
				return &interfaces[index], nil
			}
		}
	}
	return nil, fmt.Errorf("could not find the network interface for %s", ip)
}

func localIPv4s() map[string]bool {
	local := make(map[string]bool)
	interfaces, err := net.Interfaces()
	if err != nil {
		return local
	}
	for index := range interfaces {
		addresses, addressErr := interfaces[index].Addrs()
		if addressErr != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if v4 := ip.To4(); v4 != nil {
				local[v4.String()] = true
			}
		}
	}
	return local
}

func privateIPv4(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4.IsPrivate() && !v4.IsLoopback() && !v4.IsMulticast()
}
