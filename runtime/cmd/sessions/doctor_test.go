package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorWarnsWhenServeUsesFormerTailnetName(t *testing.T) {
	var output bytes.Buffer
	writeDoctorTailscale(&output, map[string]any{
		"present": true, "signedIn": true, "auto": true,
		"currentDNSName": "mac-mini-313.tail61417e.ts.net",
		"servedDNSName":  "mac-mini-8.tail61417e.ts.net",
	})
	got := output.String()
	for _, want := range []string{
		"tailscale: signed in, automatic=on",
		"warning: Tailscale Serve name mac-mini-8.tail61417e.ts.net does not match current tailnet name mac-mini-313.tail61417e.ts.net",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor Tailscale output = %q, want %q", got, want)
		}
	}
}

func TestDoctorDoesNotWarnWhenServeUsesCurrentTailnetName(t *testing.T) {
	var output bytes.Buffer
	writeDoctorTailscale(&output, map[string]any{
		"present": true, "signedIn": true, "auto": true,
		"currentDNSName": "mac-mini-313.tail61417e.ts.net",
		"servedDNSName":  "mac-mini-313.tail61417e.ts.net",
	})
	if got := output.String(); strings.Contains(got, "warning:") {
		t.Fatalf("doctor warned for matching Tailscale names: %q", got)
	}
}

func TestClassifyRunnerSpawn(t *testing.T) {
	tests := map[string]string{
		"/Applications/Sessions.app/Contents/Resources/runtime/sessions-runner": "native",
		`C:\Program Files\Sessions\sessions-runner.exe`:                         "native",
		// The Node runtime is retired. A shipped Go install cannot spawn it, so
		// calling these "dist" (healthy) or "tsx-SLOW" (a Node-era diagnosis)
		// could only mislabel some unrelated process.
		"node /work/dist/runner.js":                      "other",
		"node /work/node_modules/.bin/tsx src/runner.ts": "other",
		"/bin/zsh": "other",
		"":         "dead?",
	}
	for command, want := range tests {
		if got := classifyRunnerSpawn(command); got != want {
			t.Fatalf("classifyRunnerSpawn(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestRunnerSpawnResolvesPTYChildParent(t *testing.T) {
	fields := map[string]string{
		"command=:123": "/opt/homebrew/bin/claude",
		"ppid=:123":    "122",
		"command=:122": "/Applications/Sessions.app/Contents/Resources/runtime/sessions-runner",
	}
	lookup := func(format string, pid int) string {
		return fields[format+":"+strconv.Itoa(pid)]
	}
	if got := runnerSpawn(123, lookup); got != "native" {
		t.Fatalf("runnerSpawn() = %q, want native", got)
	}
}

func TestRunnerSpawnKeepsStructuredRunnerAndRealFaultsDistinct(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "structured runner is the recorded pid",
			fields: map[string]string{
				"command=:123": "/Applications/Sessions.app/Contents/Resources/runtime/sessions-runner",
			},
			want: "native",
		},
		{
			name: "unrelated child and parent remain a fault",
			fields: map[string]string{
				"command=:123": "/bin/zsh", "ppid=:123": "1", "command=:1": "/sbin/launchd",
			},
			want: "other",
		},
		{name: "missing process is dead", fields: map[string]string{}, want: "dead?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(format string, pid int) string {
				return test.fields[format+":"+strconv.Itoa(pid)]
			}
			if got := runnerSpawn(123, lookup); got != test.want {
				t.Fatalf("runnerSpawn() = %q, want %q", got, test.want)
			}
		})
	}
}

// A probe that cannot run on this host must not be reported as a fault: the
// Windows adapter has no per-session launchd QoS and no ps to ask what a pid is
// running, and telling that user to recreate every session would be a lie.
func TestDoctorRowOKTreatsSkippedProbesAsNeutral(t *testing.T) {
	tests := []struct {
		qos   string
		spawn string
		want  bool
	}{
		{qos: "Interactive", spawn: "native", want: true},
		{qos: probeNotApplicable, spawn: probeNotApplicable, want: true},
		{qos: probeNotApplicable, spawn: "native", want: true},
		{qos: "Interactive", spawn: probeNotApplicable, want: true},
		{qos: "Background", spawn: "native", want: false},
		{qos: "no-plist", spawn: "native", want: false},
		{qos: "Interactive", spawn: "other", want: false},
		{qos: probeNotApplicable, spawn: "dead?", want: false},
	}
	for _, test := range tests {
		if got := doctorRowOK(test.qos, test.spawn); got != test.want {
			t.Fatalf("doctorRowOK(%q, %q) = %v, want %v", test.qos, test.spawn, got, test.want)
		}
	}
}

func TestRemoteDoctorNeverAppliesLocalProcessOrLaunchAgentEvidence(t *testing.T) {
	a := &app{home: t.TempDir()}
	pattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	row := a.doctorRunnerRow(session{
		ID: "11111111-2222-4333-8444-555555555555", Tool: "claude-code", PID: 999999,
		Cols: 300, Rows: 50,
	}, false, pattern)
	if !row.OK || row.QoS != probeNotApplicable || row.Spawn != probeNotApplicable {
		t.Fatalf("remote runner was judged with local evidence: %+v", row)
	}
}

func TestRemoteDoctorTrustsDaemonRunnerGoneEvidence(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555557"
	a := &app{home: t.TempDir()}
	pattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	row := a.doctorRunnerRow(session{
		ID: id, Kind: "lane", Tool: "lane", Unreachable: true,
		UnreachableReason: "runner-lost", RunnerGone: true,
	}, false, pattern)
	if !row.Lost || row.OK || row.Action != "sessions kill "+id ||
		row.QoS != probeNotApplicable || row.Spawn != probeNotApplicable {
		t.Fatalf("remote lost runner row = %+v", row)
	}
}

func TestDoctorCallsGoneHeadlessRunnerLostAndOffersDurableClose(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555556"
	a := &app{home: t.TempDir()}
	pattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	row := a.doctorRunnerRow(session{
		ID: id, Kind: "lane", Tool: "lane", Unreachable: true,
		UnreachableReason: "runner-lost", Cols: 300, Rows: 50,
	}, true, pattern)
	if !row.Lost || row.OK || row.Action != "sessions kill "+id || row.Spawn != "dead?" {
		t.Fatalf("lost runner doctor row = %+v", row)
	}
}

func TestDoctorReadsRemoteRestoreCountFromHealth(t *testing.T) {
	deep := map[string]any{"restore": map[string]any{"pending": float64(57)}}
	if got := restorePendingFromHealth(deep); got != 57 {
		t.Fatalf("restorePendingFromHealth() = %d, want 57", got)
	}
}

func TestDoctorReportsRetiredOrphanRestoreMarkers(t *testing.T) {
	var output strings.Builder
	writeDoctorRestoreHealth(&output, map[string]any{"pending": float64(0), "retired": float64(26)})
	if got := output.String(); !strings.Contains(got, "26 orphan marker(s) retired") {
		t.Fatalf("restore health output = %q", got)
	}
}

func TestDoctorReportsBoundedRunnerArtifactCleanup(t *testing.T) {
	var output strings.Builder
	writeDoctorArtifactHealth(&output, map[string]any{"retired": float64(10), "pending": float64(170)})
	if got := output.String(); !strings.Contains(got, "10 stale set(s) retired; 170 pending") {
		t.Fatalf("runner artifact health = %q", got)
	}
}

// doctor must stay usable on a Windows host: the PTY preflight is the Unix
// terminal adapter, and its remedy names a macOS-only toolchain.
func TestDoctorProbesAreGuardedByHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		if err := ptyPreflight(); err != nil {
			t.Fatalf("ptyPreflight() ran on Windows: %v", err)
		}
		if canProbeProcessCommand() {
			t.Fatal("canProbeProcessCommand() = true on Windows, which has no ps")
		}
		if strings.Contains(doctorUnhealthyAdvice(), "QoS") ||
			strings.Contains(doctorHealthySummary(), "QoS") {
			t.Fatal("doctor reported launchd QoS on Windows")
		}
		return
	}
	if err := ptyPreflight(); err != nil {
		t.Fatalf("ptyPreflight() = %v", err)
	}
	if !canProbeProcessCommand() {
		t.Fatal("canProbeProcessCommand() = false on a Unix host")
	}
	if strings.Contains(doctorHealthySummary(), "dist") ||
		strings.Contains(doctorUnhealthyAdvice(), "tsx") {
		t.Fatal("doctor advertises an obsolete runner command")
	}
}

func TestRunnerQoSRecognizesAdoptedLegacyPlist(t *testing.T) {
	home := t.TempDir()
	id := "00000000-0000-4000-8000-000000000001"
	paths := runnerPlistPaths(home, id)
	if err := os.MkdirAll(filepath.Dir(paths[1]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[1], []byte(
		"<key>ProcessType</key>\n<string>Interactive</string>",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`<key>ProcessType</key>\s*<string>([^<]+)</string>`)
	if got := runnerQoS(home, id, pattern); got != "Interactive" {
		t.Fatalf("runnerQoS() = %q, want Interactive", got)
	}
}
