package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The single rule this whole surface exists to keep: unknown renders as "-",
// and it can never render as 0. A reader who cannot tell "Sessions does not
// know" from "this session costs nothing" will conclude a machine is idle
// while it pages.
func TestUnknownAndZeroNeverRenderAsANumber(t *testing.T) {
	if got := formatMemory(nil); got != "-" {
		t.Fatalf("unknown memory rendered as %q, want \"-\"", got)
	}
	zero := uint64(0)
	if got := formatMemory(&zero); got != "-" {
		t.Fatalf("zero memory rendered as %q; a live process always has pages resident, so "+
			"zero is another spelling of unknown and must not print as a measurement", got)
	}
	if got := formatCPUPercent(nil); got != "-" {
		t.Fatalf("unknown cpu rendered as %q, want \"-\"", got)
	}
	if got := formatCount(nil); got != "-" {
		t.Fatalf("unknown process count rendered as %q, want \"-\"", got)
	}
}

// CPU is the one place a measured zero is real and must survive: an idle agent
// burning nothing over the sample window is exactly what this column is for.
func TestMeasuredZeroCPUIsKept(t *testing.T) {
	zero := float64(0)
	if got := formatCPUPercent(&zero); got != "0.0%" {
		t.Fatalf("a measured zero rate rendered as %q, want 0.0%%", got)
	}
	tiny := 0.02
	if got := formatCPUPercent(&tiny); got != "<0.1%" {
		t.Fatalf("a rate below a tenth of a percent rendered as %q; rounding it to 0.0%% "+
			"would erase the difference between polling and idle", got)
	}
}

func TestMemoryUnits(t *testing.T) {
	for _, test := range []struct {
		bytes uint64
		want  string
	}{
		{512, "512B"},
		{4 << 10, "4K"},
		{100 << 20, "100M"},
		{3 * (1 << 30), "3.0G"},
	} {
		value := test.bytes
		if got := formatMemory(&value); got != test.want {
			t.Fatalf("formatMemory(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
}

func resourceFixtureDaemon(t *testing.T, sessions string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/api/sessions" {
			_, _ = response.Write([]byte(`{"sessions":[` + sessions + `]}`))
			return
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)
	return server
}

const measuredSession = `{"id":"41000000-0000-4000-8000-000000000001","name":"big","description":"",` +
	`"cmd":"claude","cwd":"/tmp","createdAt":1,"pid":11,"tool":"claude-code","working":false,` +
	`"lastDataAt":1,"lastUserMessageAt":null,"exited":false,"pinned":false,` +
	`"memoryBytes":1073741824,"cpuPercent":42.5,"resourceProcesses":3,"resourceSampledAt":1}`

// A session the daemon could not measure. It carries no resource fields at
// all, which is how a daemon too old to sample and a session with no live
// process both look on the wire.
const unmeasuredSession = `{"id":"41000000-0000-4000-8000-000000000002","name":"gone","description":"",` +
	`"cmd":"codex","cwd":"/tmp","createdAt":1,"pid":0,"tool":"codex","working":false,` +
	`"lastDataAt":1,"lastUserMessageAt":null,"exited":false,"pinned":false}`

func TestLSShowsResourceColumnsOnlyWhenTheySaySomething(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	server := resourceFixtureDaemon(t, measuredSession+","+unmeasuredSession)
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "ls")
	if code != 0 || stderr != "" {
		t.Fatalf("ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "MEM") || !strings.Contains(stdout, "CPU") {
		t.Fatalf("ls hid the resource columns while a session had a measurement: %q", stdout)
	}
	if !strings.Contains(stdout, "1.0G") || !strings.Contains(stdout, "42.5%") {
		t.Fatalf("ls did not render the measurement: %q", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "gone") {
			continue
		}
		fields := strings.Fields(line)
		memory, cpu := fields[len(fields)-2], fields[len(fields)-1]
		if memory != "-" || cpu != "-" {
			t.Fatalf("an unmeasured session rendered as %q/%q instead of \"-\"/\"-\": %q", memory, cpu, line)
		}
	}

	// Nothing measured anywhere: the columns are absent entirely, following
	// the PROFILE and PIN rule.
	bare := resourceFixtureDaemon(t, unmeasuredSession)
	stdout, stderr, code = runOwnershipCLI(t, bare.URL, "ls")
	if code != 0 || stderr != "" {
		t.Fatalf("bare ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "MEM") {
		t.Fatalf("the MEM column appeared with nothing measured: %q", stdout)
	}
}

func TestResourcesReportsTotalsAndNamesWhatItCouldNotMeasure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := resourceFixtureDaemon(t, measuredSession+","+unmeasuredSession)

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "resources")
	if code != 0 || stderr != "" {
		t.Fatalf("resources exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1.0G resident across 1 of 2 live sessions (3 processes)") {
		t.Fatalf("the roll-up did not state the total and the coverage it covers: %q", stdout)
	}
	if !strings.Contains(stdout, "42.5% of one core across 1 of 2 live sessions") {
		t.Fatalf("the roll-up did not state the CPU total: %q", stdout)
	}
	if !strings.Contains(stdout, "1 session(s) have no process the daemon could read") {
		t.Fatalf("the roll-up hid the sessions it could not measure, which makes the total "+
			"look like the whole machine: %q", stdout)
	}
}

func TestResourcesJSONKeepsUnknownAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := resourceFixtureDaemon(t, measuredSession+","+unmeasuredSession)

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "--json", "resources")
	if code != 0 || stderr != "" {
		t.Fatalf("json resources exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var report struct {
		Sessions         int    `json:"sessions"`
		Measured         int    `json:"measured"`
		Processes        int    `json:"processes"`
		TotalMemoryBytes uint64 `json:"totalMemoryBytes"`
		CPUMeasured      int    `json:"cpuMeasured"`
		Top              []struct {
			ID          string   `json:"id"`
			MemoryBytes uint64   `json:"memoryBytes"`
			CPUPercent  *float64 `json:"cpuPercent"`
		} `json:"top"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("undecodable resources document: %v (%q)", err, stdout)
	}
	if report.Sessions != 2 || report.Measured != 1 || report.CPUMeasured != 1 {
		t.Fatalf("counts = %+v", report)
	}
	if report.TotalMemoryBytes != 1<<30 || report.Processes != 3 {
		t.Fatalf("totals = %+v", report)
	}
	if len(report.Top) != 1 || report.Top[0].ID != "41000000-0000-4000-8000-000000000001" {
		t.Fatalf("top = %+v", report.Top)
	}
	// The unmeasured session must not appear in the biggest-consumers list as
	// a zero-cost row.
	if strings.Contains(stdout, "41000000-0000-4000-8000-000000000002") {
		t.Fatalf("an unmeasured session was listed as a consumer: %q", stdout)
	}
}

func TestResourcesRanksBiggestConsumersFirst(t *testing.T) {
	small := uint64(10 << 20)
	large := uint64(900 << 20)
	records := []sessionRecord{
		{value: session{ID: "small", MemoryBytes: &small}},
		{value: session{ID: "large", MemoryBytes: &large}},
		{value: session{ID: "unknown"}},
	}
	report := buildResourceReport(records, 10, time.Unix(0, 0))
	if len(report.Top) != 2 || report.Top[0].ID != "large" {
		t.Fatalf("top = %+v, want the largest consumer first and the unknown one excluded", report.Top)
	}
	if report.Sessions != 3 || report.Measured != 2 {
		t.Fatalf("report = %+v", report)
	}
}

// A sample is only ever as fresh as the last tick, and a reader is entitled to
// know how old the number is before acting on it.
func TestResourcesReportsSampleStaleness(t *testing.T) {
	memory := uint64(1 << 20)
	sampledAt := time.Unix(1000, 0).UnixMilli()
	records := []sessionRecord{{value: session{ID: "a", MemoryBytes: &memory, ResourceSampledAt: &sampledAt}}}
	report := buildResourceReport(records, 10, time.Unix(1042, 0))
	if report.OldestSampleAgeMS == nil || *report.OldestSampleAgeMS != 42_000 {
		t.Fatalf("oldest sample age = %v, want 42000ms", report.OldestSampleAgeMS)
	}
	var output strings.Builder
	if err := writeResourceReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "oldest sample 42s ago") {
		t.Fatalf("the report hid its own staleness: %q", output.String())
	}
}

func TestResourcesSaysNothingRatherThanZeroWhenNothingIsMeasurable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := resourceFixtureDaemon(t, unmeasuredSession)
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "resources")
	if code != 0 || stderr != "" {
		t.Fatalf("resources exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Memory and CPU are unknown, not zero") {
		t.Fatalf("a fleet with nothing measurable was not described as unknown: %q", stdout)
	}
}

// The flag a caller will reach for first, on the command that already reports
// what a session spent. Answering "unknown option" would leave them unsure the
// capability exists.
func TestUsageResourcesFlagPointsAtTheRightCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := resourceFixtureDaemon(t, measuredSession)
	stdout, stderr, code := runOwnershipCLI(t, server.URL, "usage", "--resources")
	if code == 0 {
		t.Fatalf("usage --resources succeeded: %q", stdout)
	}
	if !strings.Contains(stderr, "sessions resources") {
		t.Fatalf("the refusal did not name the command that answers it: %q", stderr)
	}
}
