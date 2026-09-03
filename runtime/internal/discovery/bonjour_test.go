package discovery

import (
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/mdns"
)

func TestCandidateFromEntryRequiresSessionsApprovalAndPrivateIPv4(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:       `Studio\ Mac._sessions._tcp.local.`,
		Host:       "sessions-abc123.local.",
		AddrV4:     net.ParseIP("192.168.4.20"),
		Port:       8787,
		InfoFields: []string{"sessions=1", "api=1", "approval=required", "transport=http"},
	}
	candidate, ok := candidateFromEntry(entry)
	if !ok {
		t.Fatal("valid Sessions entry was rejected")
	}
	if candidate.Name != "Studio Mac" ||
		candidate.Hostname != "sessions-abc123.local" ||
		candidate.Endpoint != "http://192.168.4.20:8787" ||
		candidate.Transport != "nearby" {
		t.Fatalf("candidate = %#v", candidate)
	}

	for name, mutate := range map[string]func(*mdns.ServiceEntry){
		"public address":  func(value *mdns.ServiceEntry) { value.AddrV4 = net.ParseIP("203.0.113.10") },
		"privileged port": func(value *mdns.ServiceEntry) { value.Port = 443 },
		"missing marker":  func(value *mdns.ServiceEntry) { value.InfoFields = []string{"approval=required"} },
		"automatic trust": func(value *mdns.ServiceEntry) { value.InfoFields = []string{"sessions=1", "approval=automatic"} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *entry
			copy.InfoFields = append([]string(nil), entry.InfoFields...)
			mutate(&copy)
			if _, accepted := candidateFromEntry(&copy); accepted {
				t.Fatalf("unsafe Bonjour entry was accepted: %#v", copy)
			}
		})
	}
}

func TestServiceInstanceIsBoundedAndDoesNotExposeMachineID(t *testing.T) {
	const machineID = "11111111-2222-4333-8444-555555abcdef"
	instance := serviceInstance(strings.Repeat("Studio. Mac ", 20), machineID)
	if len([]byte(instance)) > 63 {
		t.Fatalf("instance is %d bytes: %q", len([]byte(instance)), instance)
	}
	if strings.Contains(instance, machineID) || strings.Contains(instance, ".") || strings.Contains(instance, `\`) {
		t.Fatalf("instance leaked durable identity or DNS separators: %q", instance)
	}
	if !strings.HasSuffix(instance, machineIDSuffix(machineID)) || strings.Contains(instance, "abcdef") {
		t.Fatalf("instance lacks hashed disambiguator or leaked an ID suffix: %q", instance)
	}
}

func TestSanitizeServiceNamePreservesWords(t *testing.T) {
	for input, want := range map[string]string{
		"Uzair's MacBook Pro":       "Uzair's MacBook Pro",
		"MacBook-Pro":               "MacBook-Pro",
		"MacBook.Pro":               "MacBook Pro",
		`Studio.\\Mac`:              "Studio Mac",
		"workstation\x00downstairs": "workstation downstairs",
		"Studio. Mac":               "Studio Mac",
	} {
		if got := sanitizeServiceName(input); got != want {
			t.Fatalf("sanitizeServiceName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUnescapeDNSPresentation(t *testing.T) {
	for input, want := range map[string]string{
		`Studio\ Mac`:         "Studio Mac",
		`Studio\032Mac`:       "Studio Mac",
		`Sessions\194\183Mac`: "Sessions" + string([]byte{194, 183}) + "Mac",
		`literal\\slash`:      `literal\slash`,
	} {
		if got := unescapeDNSPresentation(input); got != want {
			t.Fatalf("unescapeDNSPresentation(%q) = %q, want %q", input, got, want)
		}
	}
}
