package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

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
		t.Fatal("doctor still advertises the retired Node runtime")
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
