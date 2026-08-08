package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/resource"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// tableEnumerator answers with whatever process table a test hands it, keyed
// on the PID the daemon actually assigned to its session.
type tableEnumerator struct{ build func() []resource.Process }

func (e tableEnumerator) Enumerate() ([]resource.Process, error) { return e.build(), nil }

// /api/sessions is the contract every client reads -- the CLI, the native
// shells, and any agent. If the resource fields do not travel on it they do
// not exist, and if they travel as zeros instead of being absent, every reader
// downstream reports an idle machine.
func TestSessionsRouteCarriesResourceFieldsAndOmitsUnknownOnes(t *testing.T) {
	root := t.TempDir()
	config := state.Config{
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The clock is driven by hand so the CPU rate is exact rather than
	// dependent on how fast the test machine ran.
	now := time.Unix(9000, 0)
	var pid int
	cpu := time.Second
	enumerator := tableEnumerator{build: func() []resource.Process {
		if pid == 0 {
			return nil
		}
		return []resource.Process{
			{PID: 1, PPID: 0, Name: "launchd"},
			{PID: pid, PPID: 1, Name: "sessions-runner", RSSBytes: 64 << 20, CPUTime: cpu},
			{PID: pid + 1, PPID: pid, Name: "claude", RSSBytes: 192 << 20, CPUTime: 2 * cpu},
		}
	}}

	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		ResourceEnumerator: enumerator, ResourceInterval: time.Second,
		ResourceClock: func() time.Time { return now },
	})
	t.Cleanup(manager.Close)
	server := New(config, manager, manager.Push())

	body, _ := json.Marshal(state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	created := serve(t, server, http.MethodPost, "/api/sessions", bytes.NewReader(body), "127.0.0.1:1", nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var info state.SessionInfo
	decodeBody(t, created, &info)

	// Before any sample the fields must be absent from the JSON entirely --
	// not present as 0, which a lenient client cannot distinguish from a
	// measured zero.
	raw := listedSessionJSON(t, server, info.ID)
	for _, field := range []string{"memoryBytes", "cpuPercent", "resourceProcesses", "resourceSampledAt"} {
		if _, present := raw[field]; present {
			t.Fatalf("%s was on the wire before anything was measured: %v", field, raw[field])
		}
	}

	pid = info.PID
	if pid == 0 {
		t.Fatal("the fixture session has no pid to measure")
	}

	// First sample: memory is real, the rate is not yet knowable.
	manager.SampleResources()
	raw = listedSessionJSON(t, server, info.ID)
	if got := raw["memoryBytes"]; got != float64(256<<20) {
		t.Fatalf("memoryBytes = %v, want the whole tree's %d", got, 256<<20)
	}
	if got := raw["resourceProcesses"]; got != float64(2) {
		t.Fatalf("resourceProcesses = %v, want 2", got)
	}
	if got := raw["resourceSampledAt"]; got != float64(now.UnixMilli()) {
		t.Fatalf("resourceSampledAt = %v, want %d", got, now.UnixMilli())
	}
	if _, present := raw["cpuPercent"]; present {
		t.Fatal("the first sample claimed a CPU rate; there was nothing to subtract from")
	}

	// Second sample ten seconds later: the tree burned three more seconds of
	// CPU across its two processes, so 30% of one core.
	now = now.Add(10 * time.Second)
	cpu = 2 * time.Second
	manager.SampleResources()
	raw = listedSessionJSON(t, server, info.ID)
	if got := raw["cpuPercent"]; got != float64(30) {
		t.Fatalf("cpuPercent = %v, want 30", got)
	}

	// The process disappears. The record must go back to unknown rather than
	// keeping the last figure it saw, which would be indistinguishable from a
	// live measurement.
	now = now.Add(10 * time.Second)
	pid = 0
	manager.SampleResources()
	raw = listedSessionJSON(t, server, info.ID)
	for _, field := range []string{"memoryBytes", "cpuPercent", "resourceProcesses", "resourceSampledAt"} {
		if value, present := raw[field]; present {
			t.Fatalf("%s survived the process it described as %v", field, value)
		}
	}
}

func listedSessionJSON(t *testing.T, server http.Handler, id string) map[string]any {
	t.Helper()
	response := serve(t, server, http.MethodGet, "/api/sessions", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	for _, entry := range envelope.Sessions {
		if entry["id"] == id {
			return entry
		}
	}
	t.Fatalf("session %s not in the listing", id)
	return nil
}
