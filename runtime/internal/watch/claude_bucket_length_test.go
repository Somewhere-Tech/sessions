package watch

import (
	"strings"
	"testing"
)

// The provider keeps only the first 200 characters of an encoded path and then
// appends a base-36 hash of the original, so two long directories sharing a
// prefix still land in different buckets. Sessions encoded the whole thing, so
// a deeply nested workspace resolved to a name neither side could agree on --
// the conversation was not merely somewhere else, it was nowhere findable.
//
// Every expectation below came from running the provider's own functions,
// transcribed out of the installed 2.1.222 bundle, under node. They are not
// derived from this encoder, so this test cannot pass by the encoder agreeing
// with itself.
func TestStrictEncodingMatchesTheProviderPastItsLengthLimit(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		cwd   string
		want  string
		short bool
	}{
		{
			name:  "a path at the limit keeps every character",
			cwd:   "/" + strings.Repeat("z", 199),
			want:  "-" + strings.Repeat("z", 199),
			short: true,
		},
		{
			// One character longer, and the whole shape changes.
			name: "a path one past the limit is cut and hashed",
			cwd:  "/" + strings.Repeat("z", 200),
			want: "-" + strings.Repeat("z", 199) + "-6zubpt",
		},
		{
			name: "a long ordinary path",
			cwd:  "/Users/uzair/" + strings.Repeat("a", 300),
			want: "-Users-uzair-" + strings.Repeat("a", 187) + "-hvt9og",
		},
		{
			// The hash runs over UTF-16 code units, so an astral character
			// contributes two surrogates. Iterating runes instead would give a
			// different hash and a different directory.
			name: "a long path containing an astral character",
			cwd:  "/Users/uzair/\U0001F600emoji/" + strings.Repeat("y", 240),
			want: "-Users-uzair---emoji-" + strings.Repeat("y", 179) + "-f06l9w",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := encodeClaudePathStrict(testCase.cwd)
			if got != testCase.want {
				t.Fatalf("encoded to a bucket the provider does not use\n got %q\nwant %q", got, testCase.want)
			}
			if testCase.short && len(got) > claudeProjectDirMaxLength {
				t.Fatalf("a path within the limit was truncated: %d characters", len(got))
			}
		})
	}
}

// Two workspaces sharing their first 200 characters must not share a bucket.
// That is the whole reason the provider hashes rather than simply truncating.
func TestLongPathsSharingAPrefixGetDifferentBuckets(t *testing.T) {
	prefix := "/Users/uzair/" + strings.Repeat("p", 260)
	first := encodeClaudePathStrict(prefix + "/one")
	second := encodeClaudePathStrict(prefix + "/two")
	if first == second {
		t.Fatalf("two workspaces collapsed into one bucket: %q", first)
	}
	if !strings.HasPrefix(first, "-Users-uzair-"+strings.Repeat("p", 100)) {
		t.Fatalf("truncated bucket lost its prefix: %q", first)
	}
}
