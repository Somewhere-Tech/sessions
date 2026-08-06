package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestResolveClaudeJSONL(t *testing.T) {
	const launchID = "aaaaaaaa-1111-2222-3333-444444444444"
	const otherID = "bbbbbbbb-5555-6666-7777-888888888888"

	tests := []struct {
		name       string
		files      []string
		launchUUID string
		missingDir bool
		wantFile   string
		wantReason ClaudeResolveReason
	}{
		{
			name:       "exact wins alongside another conversation",
			files:      []string{otherID + ".jsonl", launchID + ".jsonl"},
			launchUUID: launchID,
			wantFile:   launchID + ".jsonl",
			wantReason: ClaudeExact,
		},
		{
			name:       "missing launch id with sole file",
			files:      []string{otherID + ".jsonl"},
			launchUUID: launchID,
			wantFile:   otherID + ".jsonl",
			wantReason: ClaudeSoleFile,
		},
		{
			name:       "ambiguous is deliberately unresolved",
			files:      []string{otherID + ".jsonl", "cccccccc.jsonl"},
			launchUUID: launchID,
			wantReason: ClaudeAmbiguous,
		},
		{
			name:       "empty directory",
			launchUUID: launchID,
			wantReason: ClaudeEmptyDir,
		},
		{
			name:       "missing directory",
			launchUUID: launchID,
			missingDir: true,
			wantReason: ClaudeNoDir,
		},
		{
			name:       "no launch id with sole file",
			files:      []string{otherID + ".jsonl"},
			wantFile:   otherID + ".jsonl",
			wantReason: ClaudeSoleFile,
		},
		{
			name:       "no launch id with multiple files",
			files:      []string{otherID + ".jsonl", launchID + ".jsonl"},
			wantReason: ClaudeAmbiguous,
		},
		{
			name:       "non-jsonl files ignored",
			files:      []string{otherID + ".jsonl", "notes.txt", launchID + ".jsonl.tmp"},
			launchUUID: launchID,
			wantFile:   otherID + ".jsonl",
			wantReason: ClaudeSoleFile,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "project")
			if !test.missingDir {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, name := range test.files {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}

			got := ResolveClaudeJSONL(dir, test.launchUUID)
			if got.Reason != test.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, test.wantReason)
			}
			wantPath := ""
			if test.wantFile != "" {
				wantPath = filepath.Join(dir, test.wantFile)
			}
			if got.Path != wantPath {
				t.Fatalf("path = %q, want %q", got.Path, wantPath)
			}
		})
	}
}

func TestClaudeResolverHelpers(t *testing.T) {
	if got, want := EncodeClaudeCWD("/Users/example/Projects/rail-me"), "-Users-example-Projects-rail-me"; got != want {
		t.Fatalf("EncodeClaudeCWD() = %q, want %q", got, want)
	}
	if got, want := EncodeClaudeCWD(`C:\Users\example\Projects\rail-me`), "C--Users-example-Projects-rail-me"; got != want {
		t.Fatalf("EncodeClaudeCWD(Windows) = %q, want %q", got, want)
	}
	if got, want := decodeClaudeProjectName("C--Users-example-Projects-rail-me"), filepath.FromSlash("C:/Users/example/Projects/rail/me"); got != want {
		t.Fatalf("decodeClaudeProjectName(Windows) = %q, want %q", got, want)
	}
	dir := t.TempDir()
	for _, name := range []string{"one.jsonl", "two.jsonl", "notes.txt", "three.jsonl.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := ListClaudeJSONLFiles(dir)
	sort.Strings(got)
	if want := []string{"one.jsonl", "two.jsonl"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListClaudeJSONLFiles() = %v, want %v", got, want)
	}
	if normalizeCWD("/tmp") == "/private/tmp" {
		dirs := ClaudeProjectDirsUnder("/claude-projects", "/tmp")
		want := []string{"/claude-projects/-private-tmp", "/claude-projects/-tmp"}
		if !reflect.DeepEqual(dirs, want) {
			t.Fatalf("macOS /tmp project dirs = %v, want %v", dirs, want)
		}
	}
}

func TestNormalizeCWDFallsBackToClean(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "..", "still-missing")
	if got, want := normalizeCWD(missing), filepath.Clean(missing); got != want {
		t.Fatalf("normalizeCWD(missing) = %q, want Clean fallback %q", got, want)
	}
}

func TestClaudeCWDResolutionUsesRealpathAndLegacyEncoding(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	realCWD := filepath.Join(root, "private", "tmp")
	aliasCWD := filepath.Join(root, "tmp")
	if err := os.MkdirAll(realCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realCWD, aliasCWD); err != nil {
		t.Fatal(err)
	}
	// The narrow resolved and unresolved encodings must stay first and stay in
	// that order, so nothing that resolves today changes which file it picks.
	// Claude's own strict encoding follows; it collapses into the narrow pair
	// for a cwd made only of separators and alphanumerics, which is why the
	// count is a lower bound rather than an equality.
	dirs := ClaudeProjectDirsUnder(projects, aliasCWD)
	if len(dirs) < 2 {
		t.Fatalf("ClaudeProjectDirsUnder() = %v, want at least resolved and unresolved encodings", dirs)
	}
	if got, want := filepath.Base(dirs[0]), encodeClaudePath(normalizeCWD(realCWD)); got != want {
		t.Fatalf("resolved encoding = %q, want %q", got, want)
	}
	if got, want := filepath.Base(dirs[1]), encodeClaudePath(aliasCWD); got != want {
		t.Fatalf("legacy encoding = %q, want %q", got, want)
	}
	for index, dir := range dirs {
		for other := index + 1; other < len(dirs); other++ {
			if dir == dirs[other] {
				t.Fatalf("ClaudeProjectDirsUnder() repeated %q: %v", dir, dirs)
			}
		}
	}

	const launchID = "aaaaaaaa-1111-2222-3333-444444444444"
	if err := os.MkdirAll(dirs[1], 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dirs[1], launchID+".jsonl")
	if err := os.WriteFile(legacyPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ResolveClaudeCWD(projects, aliasCWD, launchID)
	if got.Reason != ClaudeExact || got.Path != legacyPath {
		t.Fatalf("legacy exact resolution = %+v, want %q", got, legacyPath)
	}

	if err := os.MkdirAll(dirs[0], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs[0], "other.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = ResolveClaudeCWD(projects, aliasCWD, "missing-launch-id")
	if got.Reason != ClaudeAmbiguous || got.Path != "" {
		t.Fatalf("cross-encoding ambiguity = %+v, want unresolved ambiguous", got)
	}
}
