package migrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// claudeBucketOracle is Claude Code's own project-directory rule, transcribed
// from the shipped bundle rather than from Sessions' encoder:
//
//	function _b(A){return A.replace(/[^a-zA-Z0-9]/g,"-")}
//
// Deriving the expected directory from Sessions' encoder would only prove the
// encoder agrees with itself. This is the independent oracle.
var claudeBucketOracle = regexp.MustCompile(`[^a-zA-Z0-9]`)

func claudeBucket(t *testing.T, cwd string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("resolve %s: %v", cwd, err)
	}
	encoded := claudeBucketOracle.ReplaceAllString(filepath.Clean(resolved), "-")
	// Past 200 characters Claude truncates and appends a hash of the unencoded
	// cwd, which neither this oracle nor watch's encoder reproduces. Fail rather
	// than assert against a name Claude would not use.
	if len(encoded) > 200 {
		t.Skipf("fixture path encodes to %d characters; Claude truncates past 200", len(encoded))
	}
	return encoded
}

func punctuatedWorkspace(t *testing.T, root string) string {
	t.Helper()
	// A dot, an underscore and a space: three characters Claude folds and the
	// narrow encoding leaves alone. Any one of them is enough to send a move
	// into a directory Claude never reads.
	cwd := filepath.Join(root, "my.work_space 1")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func claudeConversation(t *testing.T, provider, cwd string) []byte {
	t.Helper()
	record := map[string]any{"type": "user", "sessionId": provider}
	if cwd != "" {
		record["cwd"] = cwd
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func receiveClaude(t *testing.T, home, cwd, provider string, conversation []byte) ReceiveResult {
	t.Helper()
	result, err := Receive(context.Background(), ReceiveRequest{
		Tool: "claude-code", UUID: provider, Cwd: cwd, ConversationBytes: conversation,
		ResumeRecipe: []string{"claude", "--resume", provider},
	}, ReceiveOptions{Home: home})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	return result
}

// A moved conversation is only moved if the destination machine can resume it.
// Claude looks in one directory, computed by folding every non-alphanumeric
// character of the cwd; Sessions wrote into the directory produced by folding
// only separators. For /Users/u/my.work_space those are different directories,
// the move reported success, and "claude --resume <uuid>" at the destination
// found nothing.
func TestReceiveWritesTheBucketClaudeActuallyReads(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := punctuatedWorkspace(t, root)
	provider := "33333333-3333-4333-8333-333333333333"

	result := receiveClaude(t, home, cwd, provider, claudeConversation(t, provider, cwd))

	projects := filepath.Join(home, ".claude", "projects")
	want := filepath.Join(projects, claudeBucket(t, cwd), provider+".jsonl")
	if result.ConversationPath != want {
		t.Fatalf("conversation path = %s, want the bucket Claude reads %s", result.ConversationPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("conversation is not in Claude's bucket: %v", err)
	}
	// And the destination is resumable through the same resolver the daemon
	// uses, by the exact provider UUID the resume recipe carries.
	resolved := watch.ResolveClaudeCWD(projects, cwd, provider)
	if resolved.Path != want || resolved.Reason != watch.ClaudeExact {
		t.Fatalf("destination resolution = %+v, want exact %s", resolved, want)
	}
}

// A cwd made only of separators and alphanumerics encodes identically under
// both rules, so nothing that works today moves. Stated on a literal path
// because a test temp directory on this platform already contains an
// underscore, which is exactly the character the two encoders disagree about.
func TestReceiveKeepsThePlainBucketUnchanged(t *testing.T) {
	provider := "44444444-4444-4444-8444-444444444444"
	projects := filepath.Join(t.TempDir(), "absent", "projects")
	for _, cwd := range []string{"/Users/example/plainworkspace", "/var/tmp/work"} {
		got := claudeConversationDestination(projects, cwd, provider)
		want := filepath.Join(projects, watch.EncodeClaudeCWD(cwd), provider+".jsonl")
		if got != want {
			t.Fatalf("destination for %s = %s, want unchanged %s", cwd, got, want)
		}
	}
}

// Conversations moved by an earlier Sessions are sitting in the narrow bucket
// right now. Re-running the move must find that copy instead of writing a
// second one: two files with the same provider UUID in two buckets make the
// UUID match multiple Claude projects, which breaks adoption by UUID outright.
func TestReceiveReusesAConversationAlreadyMovedUnderTheNarrowEncoding(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := punctuatedWorkspace(t, root)
	provider := "55555555-5555-4555-8555-555555555555"
	conversation := claudeConversation(t, provider, cwd)

	projects := filepath.Join(home, ".claude", "projects")
	legacy := filepath.Join(projects, watch.EncodeClaudeCWD(cwd), provider+".jsonl")
	strict := filepath.Join(projects, claudeBucket(t, cwd), provider+".jsonl")
	if legacy == strict {
		t.Fatalf("fixture cwd %s does not distinguish the two encodings", cwd)
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, conversation, 0o600); err != nil {
		t.Fatal(err)
	}

	result := receiveClaude(t, home, cwd, provider, conversation)

	if result.ConversationPath != legacy || !result.AlreadyPresent {
		t.Fatalf("receive = %+v, want the existing copy at %s reported as already present", result, legacy)
	}
	if _, err := os.Stat(strict); !os.IsNotExist(err) {
		t.Fatalf("receive duplicated the conversation into %s (stat err = %v)", strict, err)
	}
	// The daemon still finds the legacy copy: resolution probes both encodings.
	if resolved := watch.ResolveClaudeCWD(projects, cwd, provider); resolved.Path != legacy {
		t.Fatalf("legacy resolution = %+v, want %s", resolved, legacy)
	}
}

// Adoption is the coupled half. Migrate writing strict-encoded buckets is only
// safe if recovery can still resolve a conversation out of either bucket, so
// both are exercised through the public adoption entry points, by path and by
// provider UUID.
func TestAdoptionResolvesConversationsInEitherBucket(t *testing.T) {
	provider := "66666666-6666-4666-8666-666666666666"
	for _, encoding := range []struct {
		name   string
		bucket func(t *testing.T, cwd string) string
	}{
		{name: "strict", bucket: claudeBucket},
		{name: "narrow", bucket: func(t *testing.T, cwd string) string { return watch.EncodeClaudeCWD(cwd) }},
	} {
		t.Run(encoding.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := punctuatedWorkspace(t, root)
			projects := filepath.Join(root, ".claude", "projects")
			dir := filepath.Join(projects, encoding.bucket(t, cwd))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, provider+".jsonl")
			if err := os.WriteFile(path, claudeConversation(t, provider, cwd), 0o600); err != nil {
				t.Fatal(err)
			}
			options := recovery.AdoptionOptions{ClaudeProjectsDir: projects}
			byPath, err := recovery.ResolveAdoption(path, options)
			if err != nil {
				t.Fatalf("adopt by path: %v", err)
			}
			if byPath.Cwd != cwd || byPath.ProviderUUID != provider {
				t.Fatalf("adoption by path = %+v, want cwd %s", byPath, cwd)
			}
			byUUID, err := recovery.ResolveAdoption(provider, options)
			if err != nil {
				t.Fatalf("adopt by uuid: %v", err)
			}
			if byUUID.Path != path || byUUID.Cwd != cwd {
				t.Fatalf("adoption by uuid = %+v, want %s", byUUID, path)
			}
		})
	}
}

// A conversation that never recorded its own cwd is the only case where the
// bucket name is consulted at all. The strict encoding cannot be inverted, so
// the neighbours are asked: they record the unencoded cwd, and one that encodes
// back to this bucket is stating what the bucket means.
func TestAdoptionTakesTheWorkspaceFromWhatNeighboursRecorded(t *testing.T) {
	root := t.TempDir()
	cwd := punctuatedWorkspace(t, root)
	projects := filepath.Join(root, ".claude", "projects")
	dir := filepath.Join(projects, claudeBucket(t, cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	neighbour := "77777777-7777-4777-8777-777777777777"
	if err := os.WriteFile(filepath.Join(dir, neighbour+".jsonl"),
		claudeConversation(t, neighbour, cwd), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := "88888888-8888-4888-8888-888888888888"
	path := filepath.Join(dir, provider+".jsonl")
	if err := os.WriteFile(path, claudeConversation(t, provider, ""), 0o600); err != nil {
		t.Fatal(err)
	}

	adoption, err := recovery.ResolveAdoption(path, recovery.AdoptionOptions{ClaudeProjectsDir: projects})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adoption.Cwd != cwd {
		t.Fatalf("adopted cwd = %q, want the workspace the bucket's neighbours recorded %q", adoption.Cwd, cwd)
	}
}

// With nothing recorded anywhere, inverting the strict bucket produces a
// directory that does not exist -- "my.work_space 1" comes back as three path
// components. Adoption must refuse rather than launch an agent there: the
// wrong cwd is not a degraded adoption, it is an agent editing the wrong repo.
func TestAdoptionRefusesAWorkspaceItCanOnlyGuess(t *testing.T) {
	root := t.TempDir()
	cwd := punctuatedWorkspace(t, root)
	projects := filepath.Join(root, ".claude", "projects")
	dir := filepath.Join(projects, claudeBucket(t, cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := "99999999-9999-4999-8999-999999999999"
	path := filepath.Join(dir, provider+".jsonl")
	if err := os.WriteFile(path, claudeConversation(t, provider, ""), 0o600); err != nil {
		t.Fatal(err)
	}

	adoption, err := recovery.ResolveAdoption(path, recovery.AdoptionOptions{ClaudeProjectsDir: projects})
	if err == nil {
		t.Fatalf("adoption invented a workspace: %+v", adoption)
	}
	if got := err.Error(); !regexp.MustCompile(`provider-unbound: cannot resolve cwd`).MatchString(got) {
		t.Fatalf("refusal = %v, want a provider-unbound cwd refusal", got)
	}
}

// The narrow bucket stays invertible, and a conversation that recorded no cwd
// there still adopts. This is the pre-existing behaviour that conversations
// moved by an earlier Sessions depend on.
func TestAdoptionStillInvertsTheNarrowBucket(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "plainworkspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(root, ".claude", "projects")
	dir := filepath.Join(projects, watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := "12121212-1212-4212-8212-121212121212"
	path := filepath.Join(dir, provider+".jsonl")
	if err := os.WriteFile(path, claudeConversation(t, provider, ""), 0o600); err != nil {
		t.Fatal(err)
	}

	adoption, err := recovery.ResolveAdoption(path, recovery.AdoptionOptions{ClaudeProjectsDir: projects})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.Cwd != resolved {
		t.Fatalf("adopted cwd = %q, want %q", adoption.Cwd, resolved)
	}
}

// End to end: a conversation moved to this machine is adoptable here. This is
// the whole product claim -- the conversation is resumable at the destination
// -- expressed as the two packages meeting.
func TestMovedConversationIsAdoptableAtTheDestination(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := punctuatedWorkspace(t, root)
	provider := "13131313-1313-4313-8313-131313131313"

	result := receiveClaude(t, home, cwd, provider, claudeConversation(t, provider, cwd))

	projects := filepath.Join(home, ".claude", "projects")
	adoption, err := recovery.ResolveAdoption(provider, recovery.AdoptionOptions{ClaudeProjectsDir: projects})
	if err != nil {
		t.Fatalf("moved conversation is not adoptable at the destination: %v", err)
	}
	if adoption.Path != result.ConversationPath || adoption.Cwd != cwd {
		t.Fatalf("adoption = %+v, want the received conversation %s in %s", adoption, result.ConversationPath, cwd)
	}
	if len(adoption.Args) == 0 || adoption.Args[len(adoption.Args)-1] != provider {
		t.Fatalf("resume recipe lost the provider identity: %+v", adoption)
	}
}
