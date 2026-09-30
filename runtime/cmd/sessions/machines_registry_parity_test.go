package main

import (
	"os"
	"path/filepath"
	"testing"
)

// savedFleetRegistryFixture is the exact registry the daemon's fleet listing is
// pinned against in internal/api. The CLI and the daemon read this one file, so
// they have to agree about which machines it contains: a directory-claimed row
// whose published address this host has no transport for costs that row, not
// the fleet.
const savedFleetRegistryFixture = `{"version":1,"machines":[
  {"alias":"b","machine_id":"machine-b","name":"Mac B","endpoint":"http://192.168.1.20:8787",
   "lan_endpoint":"http://192.168.1.20:8787","transport":"nearby","device_id":"device-b"},
  {"alias":"d","machine_id":"machine-d","name":"Mac D","endpoint":"https://mac-d.example.com",
   "tailnet_endpoint":"https://mac-d.example.com","transport":"tailnet","device_id":"device-d","source":"account"}
]}`

func TestSavedRegistryStaysReadableWhenOneRowIsUnusableHere(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	path := machineRegistryPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(savedFleetRegistryFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	registry, err := readMachineRegistry(home)
	if err != nil {
		t.Fatalf("readMachineRegistry() error = %v; the CLI must not lose the fleet over one row", err)
	}
	ids := make([]string, 0, len(registry.Machines))
	for _, machine := range registry.Machines {
		ids = append(ids, machine.MachineID)
	}
	if len(ids) != 2 || ids[0] != "machine-b" || ids[1] != "machine-d" {
		t.Fatalf("saved machines = %v, want both rows", ids)
	}

	// The unusable row keeps the address it was saved with; deciding whether a
	// route can be dialled belongs to the host that dials it, which answers 502
	// with a reason rather than dropping the machine.
	if got := registry.Machines[1].TailnetEndpoint; got != "https://mac-d.example.com" {
		t.Fatalf("directory-claimed endpoint = %q", got)
	}
}
