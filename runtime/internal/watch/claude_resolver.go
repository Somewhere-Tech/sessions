package watch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	// claudeCWDProbeLines bounds how deep the cwd probe reads into a candidate
	// transcript. Records carrying a cwd appear within the first handful of
	// lines in practice; on the development machine's transcripts the deepest
	// was well inside this.
	claudeCWDProbeLines = 64

	// claudeCWDProbeLineCap bounds one probed line. A pasted file or a large
	// tool result can make a single record enormous, and the probe must not
	// allocate proportionally to it.
	claudeCWDProbeLineCap = 1 << 20
)

// ClaudeResolveReason is the exact TypeScript resolver reason string.
type ClaudeResolveReason string

const (
	ClaudeExact     ClaudeResolveReason = "exact"
	ClaudeSoleFile  ClaudeResolveReason = "sole-file"
	ClaudeAmbiguous ClaudeResolveReason = "ambiguous"
	ClaudeEmptyDir  ClaudeResolveReason = "empty-dir"
	ClaudeNoDir     ClaudeResolveReason = "no-dir"

	// ClaudeCWDMatch resolves an otherwise ambiguous bucket by reading the
	// working directory each candidate transcript recorded for itself. See
	// resolveAmbiguousByRecordedCWD for why that is evidence and not a guess.
	ClaudeCWDMatch ClaudeResolveReason = "cwd-match"
)

// ClaudeResolution identifies the JSONL to follow. An empty Path for an
// ambiguous directory is intentional: following the wrong conversation is
// worse than showing no structured events.
type ClaudeResolution struct {
	Path   string              `json:"path"`
	Reason ClaudeResolveReason `json:"reason"`
}

// normalizeCWD resolves filesystem aliases when possible. Codex records the
// kernel-resolved cwd in session_meta on macOS (/private/tmp), while callers
// can launch the runner through an alias (/tmp). Keep Clean as the fallback
// for deleted or not-yet-created paths.
func normalizeCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	cleaned := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	return cleaned
}

// EncodeClaudeCWD matches Claude Code's project-directory convention after
// resolving filesystem aliases, which is how Claude names its project dir.
//
// This is the NARROW encoding: it folds only "/", "\" and ":". It is not what
// Claude Code actually does -- see EncodeClaudeCWDStrict -- but it stays the
// canonical Sessions encoding because other packages both write paths with it
// (internal/migrate) and invert it (internal/recovery). Changing what it
// returns would move existing files out from under those readers. Resolution
// probes both encodings instead; see ClaudeProjectDirsUnder.
func EncodeClaudeCWD(cwd string) string {
	return encodeClaudePath(normalizeCWD(cwd))
}

func encodeClaudePath(cwd string) string {
	if cwd == "" {
		return ""
	}
	// Claude sanitizes both platform path separators and the Windows drive
	// separator into dashes. Replacing only "/" makes C:\... produce an
	// invalid project-directory name on Windows and prevents transcript
	// discovery there.
	return strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(filepath.Clean(cwd))
}

// EncodeClaudeCWDStrict reproduces Claude Code's real project-directory
// encoder, which folds EVERY non-alphanumeric byte to a dash:
//
//	function _b(A){return A.replace(/[^a-zA-Z0-9]/g,"-")}
//
// That is lifted verbatim from the shipped Claude Code bundle and the same
// regex is present in the current native binary (2.1.222).
//
// The narrow encoding folds only separators, so it leaves alone characters
// Claude folds. Any cwd containing ".", "_", " ",
// "+", "~" or any other punctuation lands in a different bucket under the two
// encoders, and Sessions looking only in the narrow one misses the transcript
// entirely -- not ambiguously, but completely, while the file sits in the
// adjacent directory. /Users/uzair/pretty_tmux is the whole failure: Claude
// writes -Users-uzair-pretty-tmux and the narrow encoder looks in
// -Users-uzair-pretty_tmux.
//
// This is both a read and a write path: resolution probes it to widen the set
// of candidate directories, and internal/migrate writes a moved conversation
// into it, because a file written anywhere else is one the provider will never
// read. The encoding is lossy -- two cwds can share a bucket -- so a reader
// that needs the real working directory takes it from what the transcripts in
// that bucket recorded rather than by inverting the name.
func EncodeClaudeCWDStrict(cwd string) string {
	return encodeClaudePathStrict(normalizeCWD(cwd))
}

func encodeClaudePathStrict(cwd string) string {
	if cwd == "" {
		return ""
	}
	cleaned := filepath.Clean(cwd)
	encoded := make([]byte, 0, len(cleaned))
	for _, character := range cleaned {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			encoded = append(encoded, byte(character))
		case character > 0xFFFF:
			// The JavaScript regex runs over UTF-16 code units, so a non-BMP
			// rune is two units and folds to two dashes. Emitting one would
			// produce a bucket name one character short of the real one, which
			// misses just as completely as not widening at all.
			encoded = append(encoded, '-', '-')
		default:
			encoded = append(encoded, '-')
		}
	}
	// Past its limit the provider keeps a prefix and appends a hash of the
	// original path, so two long directories that share the first 200
	// characters still land in different buckets. Without this a deeply nested
	// workspace is not merely encoded differently, it is encoded into a name
	// no reader and no writer can agree on.
	if len(encoded) > claudeProjectDirMaxLength {
		return string(encoded[:claudeProjectDirMaxLength]) + "-" + claudeProjectDirHash(cleaned)
	}
	return string(encoded)
}

// claudeProjectDirMaxLength is the prefix the provider keeps before it starts
// hashing.
const claudeProjectDirMaxLength = 200

// claudeProjectDirHash reproduces the provider's suffix for an over-long path.
//
// The original is `t = (t << 5) - t + charCodeAt(i) | 0`, i.e. a 31-multiplier
// hash truncated to a signed 32-bit integer, then Math.abs, then base 36. Two
// details decide whether this agrees with it. The iteration is over UTF-16
// code units rather than runes, so a non-BMP character contributes its two
// surrogates. And the absolute value is taken in a wider type, because the
// most negative 32-bit integer has no positive counterpart -- JavaScript
// numbers do not wrap there and neither can this.
func claudeProjectDirHash(path string) string {
	var hash int32
	for _, unit := range utf16.Encode([]rune(path)) {
		hash = hash*31 + int32(unit)
	}
	magnitude := int64(hash)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	return strconv.FormatInt(magnitude, 36)
}

// ClaudeProjectsDir returns Claude Code's per-user project-session root.
// Resolve it at call time so tests and scratch daemons can use a fixture HOME.
func ClaudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// ClaudeProjectDir returns Claude's default project directory for cwd. The
// home directory is resolved only when this function is called.
func ClaudeProjectDir(cwd string) (string, error) {
	projects, err := ClaudeProjectsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(projects, EncodeClaudeCWD(cwd)), nil
}

// ClaudeProjectDirs returns the resolved project directory first and also the
// legacy unresolved encoding when it differs. Older Sessions/Claude sessions
// may have been persisted under that alias, so production readers must probe
// both without guessing between unrelated conversations.
func ClaudeProjectDirs(cwd string) ([]string, error) {
	projects, err := ClaudeProjectsDir()
	if err != nil {
		return nil, err
	}
	return ClaudeProjectDirsUnder(projects, cwd), nil
}

// ClaudeProjectDirsUnder is the fixture/root-overridable form used by the
// watcher and recovery engine.
//
// Four candidates, in decreasing order of how likely they are to be the bucket
// Sessions itself wrote: the alias-resolved and raw narrow encodings, then the
// same pair under Claude's real encoder. The narrow pair stays first so nothing
// that resolves today changes which file it resolves to. The strict pair is
// what finds a transcript for any cwd containing punctuation Claude folds and
// Sessions does not -- without it those sessions resolve to "no-dir" while the
// conversation sits in the next directory along.
//
// Duplicates are collapsed: for a cwd of only separators and alphanumerics all
// four encodings coincide, which is the common case.
func ClaudeProjectDirsUnder(projects, cwd string) []string {
	candidates := []string{
		EncodeClaudeCWD(cwd),
		encodeClaudePath(cwd),
		EncodeClaudeCWDStrict(cwd),
		encodeClaudePathStrict(cwd),
	}
	dirs := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		dir := filepath.Join(projects, candidate)
		if _, duplicate := seen[dir]; duplicate {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	return dirs
}

// ListClaudeJSONLFiles returns JSONL basenames in dir. Missing and unreadable
// directories produce an empty list, matching the TypeScript helper.
func ListClaudeJSONLFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, entry.Name())
		}
	}
	return files
}

// ResolveClaudeJSONL applies the normative exact/sole/ambiguous policy.
func ResolveClaudeJSONL(dir, launchUUID string) ClaudeResolution {
	return resolveClaudeJSONLDirs([]string{dir}, launchUUID)
}

// ResolveClaudeCWD applies the exact/sole/ambiguous policy across both the
// realpath-derived Claude directory and the legacy unresolved cwd encoding.
// An exact launch UUID wins in either directory. Without an exact match there
// must be exactly one JSONL across all existing candidates.
func ResolveClaudeCWD(projects, cwd, launchUUID string) ClaudeResolution {
	return resolveClaudeJSONLDirsForCWD(ClaudeProjectDirsUnder(projects, cwd), launchUUID, cwd)
}

func resolveClaudeJSONLDirs(dirs []string, launchUUID string) ClaudeResolution {
	return resolveClaudeJSONLDirsForCWD(dirs, launchUUID, "")
}

func resolveClaudeJSONLDirsForCWD(dirs []string, launchUUID, cwd string) ClaudeResolution {
	sawDir := false
	files := make([]string, 0)
	seen := make(map[string]struct{}, 4)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		sawDir = true
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if launchUUID != "" && entry.Name() == launchUUID+".jsonl" {
				return ClaudeResolution{Path: path, Reason: ClaudeExact}
			}
			// Two encodings can name the same directory on a case-insensitive
			// filesystem, and counting one transcript twice would turn a sole
			// file into a false ambiguity.
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			files = append(files, path)
		}
	}
	if !sawDir {
		return ClaudeResolution{Reason: ClaudeNoDir}
	}
	switch len(files) {
	case 0:
		return ClaudeResolution{Reason: ClaudeEmptyDir}
	case 1:
		return ClaudeResolution{Path: files[0], Reason: ClaudeSoleFile}
	default:
		return resolveAmbiguousByRecordedCWD(files, cwd)
	}
}

// resolveAmbiguousByRecordedCWD breaks a tie using what each candidate
// transcript says about itself.
//
// The project bucket is a lossy encoding of the working directory: every
// non-alphanumeric byte becomes a dash, so /Users/u/a-b, /Users/u/a/b,
// /Users/u/a.b and /Users/u/a_b all land in -Users-u-a-b. Two projects sharing
// a bucket makes every session in it ambiguous, and an ambiguous bucket
// currently resolves to no path at all -- the conversation reads as missing
// while the file is sitting right there. On this machine
// /Users/uzair/pretty-PTY-desktop-ux and /Users/uzair/pretty-PTY/desktop-ux
// share the bucket -Users-uzair-pretty-PTY-desktop-ux.
//
// Claude stamps the real, unencoded cwd into the transcript's own records, so
// the collision the directory name lost is recoverable from the file contents.
// Filtering candidates by it is reading recorded fact, not guessing: a
// transcript that declares a different cwd is definitively not this session's.
//
// The bar stays exactly where the rest of the resolver puts it. One surviving
// candidate resolves; zero or several stay ambiguous and resolve to no path,
// because following the wrong conversation is worse than showing none.
func resolveAmbiguousByRecordedCWD(files []string, cwd string) ClaudeResolution {
	target := normalizeCWD(cwd)
	if target == "" {
		return ClaudeResolution{Reason: ClaudeAmbiguous}
	}
	matches := make([]string, 0, 1)
	for _, path := range files {
		recorded, ok := claudeTranscriptCWD(path)
		if !ok {
			// A transcript that never declares a cwd cannot be excluded, but it
			// cannot be selected either. Leaving it out of matches while it is
			// still a real candidate would let a single match win on the
			// strength of the other file being unreadable, so bail out.
			return ClaudeResolution{Reason: ClaudeAmbiguous}
		}
		if recorded == target {
			matches = append(matches, path)
		}
	}
	if len(matches) == 1 {
		return ClaudeResolution{Path: matches[0], Reason: ClaudeCWDMatch}
	}
	return ClaudeResolution{Reason: ClaudeAmbiguous}
}

// ClaudeTranscriptFacts is what a transcript recorded about its own launch.
// Every field is optional; a zero value means the records in the probed prefix
// did not carry it, which is not the same as the conversation having no answer.
type ClaudeTranscriptFacts struct {
	CWD          string
	Entrypoint   string
	PromptSource string
	Version      string
	GitBranch    string
	Sidechain    bool
}

// probeClaudeTranscript reads what a transcript recorded about itself, over a
// bounded prefix. Not every record carries every field -- the opening summary
// and meta lines often carry none -- so a prefix is scanned rather than only
// the first line. The bound matters: this runs on an ambiguous bucket, which
// may hold a gigabyte-scale transcript (the largest on the development machine
// is 1.1 GB), and resolution must stay cheap enough to run on every watcher
// tick.
//
// One reader answers both questions asked of these files. The working directory
// breaks a bucket collision; the entrypoint says which surface the conversation
// was started from. They live on the same records, so reading them separately
// would double the cost of every listing for nothing.
func probeClaudeTranscript(path string) (ClaudeTranscriptFacts, bool) {
	file, err := os.Open(path)
	if err != nil {
		return ClaudeTranscriptFacts{}, false
	}
	defer file.Close()

	var facts ClaudeTranscriptFacts
	found := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), claudeCWDProbeLineCap)
	for line := 0; line < claudeCWDProbeLines && scanner.Scan(); line++ {
		var probe struct {
			CWD          string `json:"cwd"`
			Entrypoint   string `json:"entrypoint"`
			PromptSource string `json:"promptSource"`
			Version      string `json:"version"`
			GitBranch    string `json:"gitBranch"`
			Sidechain    bool   `json:"isSidechain"`
		}
		if json.Unmarshal(scanner.Bytes(), &probe) != nil {
			continue
		}
		for _, field := range []struct {
			into  *string
			value string
		}{
			{&facts.CWD, probe.CWD},
			{&facts.Entrypoint, probe.Entrypoint},
			{&facts.PromptSource, probe.PromptSource},
			{&facts.Version, probe.Version},
			{&facts.GitBranch, probe.GitBranch},
		} {
			if *field.into == "" && strings.TrimSpace(field.value) != "" {
				*field.into = strings.TrimSpace(field.value)
				found = true
			}
		}
		if probe.Sidechain {
			facts.Sidechain = true
			found = true
		}
		if facts.CWD != "" && facts.Entrypoint != "" && facts.PromptSource != "" {
			break
		}
	}
	return facts, found
}

// ReadClaudeTranscriptFacts is the exported probe. Callers that need the
// working directory a conversation actually ran in use this rather than
// inverting the project-directory name, which is a lossy encoding and therefore
// a guess.
func ReadClaudeTranscriptFacts(path string) (ClaudeTranscriptFacts, bool) {
	return probeClaudeTranscript(path)
}

// ReadClaudeConversationSurface returns where a Claude conversation was started
// from, out of the same bounded probe used to resolve ambiguous buckets.
func ReadClaudeConversationSurface(path string) (ConversationSurface, bool) {
	facts, ok := probeClaudeTranscript(path)
	if !ok {
		return ConversationSurface{}, false
	}
	surface := ClaudeSurface(facts.Entrypoint, facts.PromptSource, facts.Version, facts.Sidechain)
	return surface, surface.Known()
}

func claudeTranscriptCWD(path string) (string, bool) {
	facts, ok := probeClaudeTranscript(path)
	if !ok || facts.CWD == "" {
		return "", false
	}
	return normalizeCWD(facts.CWD), true
}

// ResolveJSONLPath is a compatibility name matching the TypeScript resolver.
func ResolveJSONLPath(dir, launchUUID string) ClaudeResolution {
	return ResolveClaudeJSONL(dir, launchUUID)
}
