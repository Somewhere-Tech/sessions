package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

// Adoption writes to the ledger; it does not reconcile every lane the ledger
// has ever recorded. The reconciliation report asks launchd about each lane
// with one `launchctl print`, so a launchctl on PATH that only counts its calls
// shows whether a resume paid for a report. The report request at the end is
// the control: it proves the counter sees the probes it is meant to see.
func TestAdoptionDoesNotProbeEveryRecordedLane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ledgerPath := filepath.Join(t.TempDir(), "lanes.sqlite3")
	t.Setenv("SESSIONS_LEDGER_PATH", ledgerPath)
	calls := filepath.Join(t.TempDir(), "launchctl.calls")
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\nexit 113\n"
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	const lanes = 3
	recordFixtureLanes(t, ledgerPath, lanes)
	daemon := newTestDaemon(t)
	headers := http.Header{"Authorization": {"Bearer " + testToken}, "Content-Type": {"application/json"}}

	adopt := serve(t, daemon.handler, http.MethodPost, "/api/recovery/adopt",
		strings.NewReader(`{"historyId":"00000000-0000-4000-8000-999999999999"}`), "127.0.0.1:1", headers)
	if adopt.Code != http.StatusNotFound {
		t.Fatalf("adopt status=%d body=%s, want the unknown history refused", adopt.Code, adopt.Body.String())
	}
	if probes := launchctlCalls(t, calls); probes != 0 {
		t.Fatalf("adoption made %d launchd probes, want none: it needs the ledger, not a report", probes)
	}

	report := serve(t, daemon.handler, http.MethodGet, "/api/recovery", nil, "127.0.0.1:1", headers)
	if report.Code != http.StatusOK {
		t.Fatalf("report status=%d body=%s", report.Code, report.Body.String())
	}
	var body recovery.Report
	decodeBody(t, report, &body)
	if len(body.Lanes) != lanes {
		t.Fatalf("report has %d lanes, want %d", len(body.Lanes), lanes)
	}
	// A probe has a 350 ms production deadline. Under concurrent package
	// tests it can expire before the counting shell starts. This control only
	// needs to establish that the counter sees report probes, not require every
	// child process to start within that deadline. Adoption must still make zero.
	if probes := launchctlCalls(t, calls); probes == 0 {
		t.Fatal("report made no launchd probes; the counter is not seeing probes")
	}
}

func recordFixtureLanes(t *testing.T, path string, count int) {
	t.Helper()
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id, AtMS: int64(1_790_000_000_000 + index)},
			Name: "fixture lane", Kind: "session", Tool: "shell", Cwd: "/tmp",
			CreatorKind: ledger.CreatorExternal, CreatorID: "adopt-probe-test",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func launchctlCalls(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(body), "\n")
}
