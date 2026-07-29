package watch

import (
	"os"
	"path/filepath"
	"strings"
)

// ClaudeResolveReason is the exact TypeScript resolver reason string.
type ClaudeResolveReason string

const (
	ClaudeExact     ClaudeResolveReason = "exact"
	ClaudeSoleFile  ClaudeResolveReason = "sole-file"
	ClaudeAmbiguous ClaudeResolveReason = "ambiguous"
	ClaudeEmptyDir  ClaudeResolveReason = "empty-dir"
	ClaudeNoDir     ClaudeResolveReason = "no-dir"
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
func ClaudeProjectDirsUnder(projects, cwd string) []string {
	resolved := filepath.Join(projects, EncodeClaudeCWD(cwd))
	unresolved := filepath.Join(projects, encodeClaudePath(cwd))
	if unresolved == resolved {
		return []string{resolved}
	}
	return []string{resolved, unresolved}
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
	return resolveClaudeJSONLDirs(ClaudeProjectDirsUnder(projects, cwd), launchUUID)
}

func resolveClaudeJSONLDirs(dirs []string, launchUUID string) ClaudeResolution {
	sawDir := false
	files := make([]string, 0)
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
		return ClaudeResolution{Reason: ClaudeAmbiguous}
	}
}

// ResolveJSONLPath is a compatibility name matching the TypeScript resolver.
func ResolveJSONLPath(dir, launchUUID string) ClaudeResolution {
	return ResolveClaudeJSONL(dir, launchUUID)
}
