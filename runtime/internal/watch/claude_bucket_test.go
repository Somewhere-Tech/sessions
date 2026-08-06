package watch

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEncodeClaudeCWDStrictMatchesClaudeRegex pins the encoder against Claude
// Code's own, which is
//
//	function _b(A){return A.replace(/[^a-zA-Z0-9]/g,"-")}
//
// read out of the shipped bundle. The narrow encoder's output is asserted
// alongside it so the divergence is visible rather than implied: every case
// where the two differ is a bucket Sessions would never have looked in.
func TestEncodeClaudeCWDStrictMatchesClaudeRegex(t *testing.T) {
	cases := []struct {
		cwd    string
		strict string
		narrow string
	}{
		// The common case: the two agree, which is why the bug went unnoticed.
		{"/Users/uzair/pretty-PTY", "-Users-uzair-pretty-PTY", "-Users-uzair-pretty-PTY"},
		// Underscore. Claude folds it, Sessions did not.
		{"/Users/uzair/pretty_tmux", "-Users-uzair-pretty-tmux", "-Users-uzair-pretty_tmux"},
		// Dot, as in any versioned or dotted directory name.
		{"/Users/uzair/site.com", "-Users-uzair-site-com", "-Users-uzair-site.com"},
		// Space, which is ordinary on macOS.
		{"/Users/uzair/My Project", "-Users-uzair-My-Project", "-Users-uzair-My Project"},
		// Mixed punctuation.
		{"/tmp/a+b~c", "-tmp-a-b-c", "-tmp-a+b~c"},
		{"/tmp/v1.2.3_final", "-tmp-v1-2-3-final", "-tmp-v1.2.3_final"},
	}
	for _, testCase := range cases {
		if got := encodeClaudePathStrict(testCase.cwd); got != testCase.strict {
			t.Errorf("strict(%q) = %q, want %q", testCase.cwd, got, testCase.strict)
		}
		if got := encodeClaudePath(testCase.cwd); got != testCase.narrow {
			t.Errorf("narrow(%q) = %q, want %q", testCase.cwd, got, testCase.narrow)
		}
	}
}

// TestEncodeClaudeCWDStrictHandlesNonBMPRunes covers the UTF-16 detail. The
// JavaScript regex counts code units, so an emoji is two of them and folds to
// two dashes; a BMP accent is one.
func TestEncodeClaudeCWDStrictHandlesNonBMPRunes(t *testing.T) {
	if got, want := encodeClaudePathStrict("/tmp/a\U0001F600b"), "-tmp-a--b"; got != want {
		t.Errorf("non-BMP rune: got %q, want %q", got, want)
	}
	if got, want := encodeClaudePathStrict("/tmp/café"), "-tmp-caf-"; got != want {
		t.Errorf("BMP rune: got %q, want %q", got, want)
	}
}

// TestResolveFindsTranscriptInStrictBucket is the miss the narrow encoder
// caused. Claude writes the transcript into the strict bucket; Sessions looked
// only in the narrow one and reported no-dir while the file was on disk.
func TestResolveFindsTranscriptInStrictBucket(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	cwd := filepath.Join(root, "pretty_tmux")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	narrow := filepath.Join(projects, EncodeClaudeCWD(cwd))
	strict := filepath.Join(projects, EncodeClaudeCWDStrict(cwd))
	if narrow == strict {
		t.Fatalf("fixture cwd %q must encode differently under the two encoders", cwd)
	}
	if err := os.MkdirAll(strict, 0o755); err != nil {
		t.Fatal(err)
	}
	const providerID = "bbbbbbbb-1111-2222-3333-444444444444"
	transcript := filepath.Join(strict, providerID+".jsonl")
	writeSessionEvents(t, transcript, conversationEvents("in the strict bucket"), false)

	// Sole file, with no launch UUID at all.
	sole := ResolveClaudeCWD(projects, cwd, "")
	if sole.Path != transcript || sole.Reason != ClaudeSoleFile {
		t.Fatalf("resolution = %+v, want %q", sole, transcript)
	}
	// And by exact id.
	exact := ResolveClaudeCWD(projects, cwd, providerID)
	if exact.Path != transcript || exact.Reason != ClaudeExact {
		t.Fatalf("exact resolution = %+v, want %q", exact, transcript)
	}
}

// TestResolveSplitsCollapsedBucketByRecordedCWD is the project-bucket collapse.
// Two different working directories encode to one bucket, so every session in
// it is ambiguous and resolves to nothing -- the conversation reads as missing
// while its file sits in the directory. Each transcript records its own real
// cwd, so the collision the directory name lost is recoverable from the files.
func TestResolveSplitsCollapsedBucketByRecordedCWD(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")

	// The real pair from this machine, reproduced under a temp root:
	// <root>/pretty-PTY-desktop-ux and <root>/pretty-PTY/desktop-ux.
	flat := filepath.Join(root, "pretty-PTY-desktop-ux")
	nested := filepath.Join(root, "pretty-PTY", "desktop-ux")
	for _, dir := range []string{flat, nested} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bucket := filepath.Join(projects, EncodeClaudeCWD(flat))
	if other := filepath.Join(projects, EncodeClaudeCWD(nested)); other != bucket {
		t.Fatalf("fixture must collide: %q vs %q", bucket, other)
	}
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}

	flatPath := filepath.Join(bucket, "cccccccc-1111-2222-3333-444444444444.jsonl")
	nestedPath := filepath.Join(bucket, "dddddddd-1111-2222-3333-444444444444.jsonl")
	writeSessionEvents(t, flatPath, cwdConversation(flat, "flat project work"), false)
	writeSessionEvents(t, nestedPath, cwdConversation(nested, "nested project work"), false)

	// Without a cwd there is nothing to split on, and refusing to guess is
	// still correct.
	if bare := ResolveClaudeJSONL(bucket, ""); bare.Path != "" || bare.Reason != ClaudeAmbiguous {
		t.Fatalf("bucket without a cwd = %+v, want ambiguous", bare)
	}

	// With one, each session finds its own transcript.
	flatResolution := ResolveClaudeCWD(projects, flat, "")
	if flatResolution.Path != flatPath || flatResolution.Reason != ClaudeCWDMatch {
		t.Fatalf("flat resolution = %+v, want %q", flatResolution, flatPath)
	}
	nestedResolution := ResolveClaudeCWD(projects, nested, "")
	if nestedResolution.Path != nestedPath || nestedResolution.Reason != ClaudeCWDMatch {
		t.Fatalf("nested resolution = %+v, want %q", nestedResolution, nestedPath)
	}
}

// TestResolveStaysAmbiguousWhenCWDCannotDecide keeps the bar where the rest of
// the resolver puts it. Two transcripts from the SAME cwd are genuinely
// indistinguishable, and following the wrong one is worse than following none.
func TestResolveStaysAmbiguousWhenCWDCannotDecide(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(projects, EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionEvents(t, filepath.Join(bucket, "1eeeeeee-1111-2222-3333-444444444444.jsonl"),
		cwdConversation(cwd, "one"), false)
	writeSessionEvents(t, filepath.Join(bucket, "2eeeeeee-1111-2222-3333-444444444444.jsonl"),
		cwdConversation(cwd, "two"), false)

	if got := ResolveClaudeCWD(projects, cwd, ""); got.Path != "" || got.Reason != ClaudeAmbiguous {
		t.Fatalf("resolution = %+v, want ambiguous rather than a guess", got)
	}
}

// TestResolveStaysAmbiguousWhenACandidateDeclaresNoCWD covers the partial
// evidence case. A candidate that never records a cwd cannot be excluded, so
// letting the other one win would be selecting on the strength of an unreadable
// file rather than on evidence.
func TestResolveStaysAmbiguousWhenACandidateDeclaresNoCWD(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(projects, EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionEvents(t, filepath.Join(bucket, "3eeeeeee-1111-2222-3333-444444444444.jsonl"),
		cwdConversation(cwd, "declares its cwd"), false)
	// conversationEvents carries no cwd field at all.
	writeSessionEvents(t, filepath.Join(bucket, "4eeeeeee-1111-2222-3333-444444444444.jsonl"),
		conversationEvents("silent about its cwd"), false)

	if got := ResolveClaudeCWD(projects, cwd, ""); got.Path != "" || got.Reason != ClaudeAmbiguous {
		t.Fatalf("resolution = %+v, want ambiguous", got)
	}
}

// TestExactLaunchUUIDStillWinsOverCWDMatching keeps the precedence intact: an
// exact provider-id match is the strongest evidence there is and must not be
// second-guessed by content probing.
func TestExactLaunchUUIDStillWinsOverCWDMatching(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, ".claude", "projects")
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := filepath.Join(projects, EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	const wanted = "5eeeeeee-1111-2222-3333-444444444444"
	wantedPath := filepath.Join(bucket, wanted+".jsonl")
	// Deliberately record a DIFFERENT cwd, so cwd matching would reject it.
	writeSessionEvents(t, wantedPath, cwdConversation(filepath.Join(root, "elsewhere"), "moved here"), false)
	writeSessionEvents(t, filepath.Join(bucket, "6eeeeeee-1111-2222-3333-444444444444.jsonl"),
		cwdConversation(cwd, "the local one"), false)

	got := ResolveClaudeCWD(projects, cwd, wanted)
	if got.Path != wantedPath || got.Reason != ClaudeExact {
		t.Fatalf("resolution = %+v, want the exact id match %q", got, wantedPath)
	}
}

// cwdConversation is conversationEvents with the cwd Claude stamps into its
// records, which is the evidence the collapsed-bucket split reads.
func cwdConversation(cwd string, texts ...string) []SessionEvent {
	events := conversationEvents(texts...)
	for _, event := range events {
		event["cwd"] = cwd
	}
	return events
}
