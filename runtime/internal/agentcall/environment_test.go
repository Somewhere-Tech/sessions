package agentcall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostileEnvironment is the set the package doc promises cannot reach the
// child: credentials that make the call billable, and endpoint overrides that
// would send the prompt somewhere the user did not choose. The old denylist
// stripped only the first two.
var hostileEnvironment = map[string]string{
	"ANTHROPIC_API_KEY":              "sk-ant-should-not-leak",
	"ANTHROPIC_AUTH_TOKEN":           "should-not-leak-auth-token",
	"ANTHROPIC_BASE_URL":             "https://attacker.example",
	"ANTHROPIC_CUSTOM_HEADERS":       "X-Evil: 1",
	"ANTHROPIC_MODEL":                "some-billed-model",
	"CLAUDE_CODE_USE_BEDROCK":        "1",
	"CLAUDE_CODE_USE_VERTEX":         "1",
	"AWS_ACCESS_KEY_ID":              "AKIAsomething",
	"AWS_SECRET_ACCESS_KEY":          "should-not-leak-aws-secret",
	"AWS_SESSION_TOKEN":              "should-not-leak-aws-session",
	"AWS_REGION":                     "us-east-1",
	"AWS_PROFILE":                    "billing",
	"GOOGLE_APPLICATION_CREDENTIALS": "/tmp/creds.json",
	"GOOGLE_CLOUD_PROJECT":           "billed-project",
	"GOOGLE_API_KEY":                 "should-not-leak-google-key",
	"CLOUD_ML_REGION":                "us-central1",
	"OPENAI_API_KEY":                 "sk-should-not-leak",
	"OPENAI_BASE_URL":                "https://attacker.example/v1",
	"OPENAI_API_BASE":                "https://attacker.example/v1",
	"OPENAI_ORGANIZATION":            "org-should-not-leak",
	"OPENAI_PROJECT":                 "proj-should-not-leak",
	"LD_PRELOAD":                     "/tmp/evil.so",
	"DYLD_INSERT_LIBRARIES":          "/tmp/evil.dylib",
	"NODE_OPTIONS":                   "--require /tmp/evil.js",
}

func environmentNames(t *testing.T, entries []string) map[string]string {
	t.Helper()
	named := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("environment entry %q has no '='", entry)
		}
		named[name] = value
	}
	return named
}

func TestEnvironmentDropsEveryProviderCredentialAndEndpointOverride(t *testing.T) {
	for name, value := range hostileEnvironment {
		t.Setenv(name, value)
	}
	named := environmentNames(t, Environment())
	for name := range hostileEnvironment {
		if value, present := named[name]; present {
			t.Errorf("%s reached the child as %q; the package promises an agent call cannot be billed or redirected", name, value)
		}
	}
	joined := strings.Join(Environment(), "\n")
	for _, secret := range []string{"should-not-leak", "attacker.example"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("child environment still contains %q:\n%s", secret, joined)
		}
	}
}

// An allowlist that also drops a lowercase alias would be trivially bypassed by
// a vendor shipping one, so the match is case-insensitive in both directions.
func TestEnvironmentDropsCaseVariantsOfProviderVariables(t *testing.T) {
	t.Setenv("anthropic_auth_token", "should-not-leak-lowercase")
	t.Setenv("Anthropic_Base_Url", "https://attacker.example")
	joined := strings.Join(Environment(), "\n")
	if strings.Contains(joined, "should-not-leak-lowercase") || strings.Contains(joined, "attacker.example") {
		t.Fatalf("a case variant reached the child:\n%s", joined)
	}
}

func TestEnvironmentKeepsWhatTheCLINeedsToStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	named := environmentNames(t, Environment())
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "HTTPS_PROXY", "PATH"} {
		if named[name] == "" {
			t.Errorf("%s did not reach the child; the CLI cannot find the user's existing sign-in without it", name)
		}
	}
	if runtime.GOOS == "windows" {
		if named["SystemRoot"] == "" && named["SYSTEMROOT"] == "" {
			t.Error("SystemRoot did not reach the child; a Node-based CLI cannot start without it")
		}
	} else if named["HOME"] == "" {
		t.Error("HOME did not reach the child")
	}
}

func TestEnvironmentExportsExactlyOnePATH(t *testing.T) {
	t.Setenv("PATH", "/inherited/bin")
	count := 0
	for _, entry := range Environment() {
		if strings.HasPrefix(strings.ToUpper(entry), "PATH=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("child received %d PATH entries, want exactly 1", count)
	}
}

// The old code joined with ":" unconditionally. On Windows that fuses the
// prefix and the first inherited entry into one unusable path, split at the
// drive colon, which loses the managed Sessions directory.
func TestChildPATHUsesThePlatformListSeparator(t *testing.T) {
	separator := string(os.PathListSeparator)
	inherited := "C:\\Sessions\\bin"
	if runtime.GOOS != "windows" {
		inherited = "/managed/sessions/bin"
	}
	built := childPATH(inherited)
	entries := filepath.SplitList(built)
	if len(entries) < 2 {
		t.Fatalf("childPATH(%q) = %q, which splits into %d entries", inherited, built, len(entries))
	}
	if entries[len(entries)-1] != inherited {
		t.Fatalf("childPATH lost or mangled the inherited entry: got %q, want it to end with %q", built, inherited)
	}
	for _, entry := range entries {
		if entry == "" {
			t.Fatalf("childPATH produced an empty entry: %q", built)
		}
	}
	if !strings.Contains(built, separator) {
		t.Fatalf("childPATH(%q) = %q, want entries joined by %q", inherited, built, separator)
	}
	for _, directory := range providerSearchDirectories() {
		if !containsEntry(entries, directory) {
			t.Errorf("childPATH dropped the provider install directory %q: %q", directory, built)
		}
	}
}

func TestChildPATHWithoutAnInheritedPATH(t *testing.T) {
	built := childPATH("")
	if built == "" {
		t.Fatal("childPATH(\"\") is empty; the child would have no PATH at all")
	}
	if strings.HasSuffix(built, string(os.PathListSeparator)) {
		t.Fatalf("childPATH(\"\") = %q, want no trailing separator", built)
	}
}

func TestProviderSearchDirectoriesArePlatformAppropriate(t *testing.T) {
	directories := providerSearchDirectories()
	if len(directories) == 0 {
		t.Fatal("no provider search directories; the Executable fallback would be dead code")
	}
	for _, directory := range directories {
		if !filepath.IsAbs(directory) {
			t.Errorf("provider search directory %q is not absolute", directory)
		}
		if runtime.GOOS == "windows" && strings.HasPrefix(directory, "/usr") {
			t.Errorf("Windows search list contains the Unix path %q", directory)
		}
	}
}

func TestExecutableCandidatesCoverPATHEXTOnWindows(t *testing.T) {
	candidates := executableCandidates(filepath.Join("dir", "claude"))
	if runtime.GOOS != "windows" {
		if len(candidates) != 1 {
			t.Fatalf("candidates = %#v, want the bare path on %s", candidates, runtime.GOOS)
		}
		return
	}
	joined := strings.ToLower(strings.Join(candidates, " "))
	for _, extension := range []string{".exe", ".cmd"} {
		if !strings.Contains(joined, extension) {
			t.Errorf("candidates = %#v, want a %s candidate", candidates, extension)
		}
	}
}

func TestIsExecutableFileRejectsDirectoriesAndMissingPaths(t *testing.T) {
	directory := t.TempDir()
	if isExecutableFile(directory) {
		t.Error("isExecutableFile accepted a directory")
	}
	if isExecutableFile(filepath.Join(directory, "absent")) {
		t.Error("isExecutableFile accepted a missing path")
	}
	regular := filepath.Join(directory, "claude")
	if err := os.WriteFile(regular, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(regular) {
		t.Error("isExecutableFile rejected an executable regular file")
	}
}

func containsEntry(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}
