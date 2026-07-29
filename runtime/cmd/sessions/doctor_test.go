package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestClassifyRunnerSpawn(t *testing.T) {
	tests := map[string]string{
		"/Applications/Sessions.app/Contents/Resources/runtime/sessions-runner": "native",
		"node /work/dist/runner.js":                      "dist",
		"node /work/node_modules/.bin/tsx src/runner.ts": "tsx-SLOW",
		"/bin/zsh": "other",
		"":         "dead?",
	}
	for command, want := range tests {
		if got := classifyRunnerSpawn(command); got != want {
			t.Fatalf("classifyRunnerSpawn(%q) = %q, want %q", command, got, want)
		}
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
