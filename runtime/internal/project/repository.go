package project

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repository is what a folder says about itself: the main checkout it belongs
// to, the GitHub repository its origin points at, and the Somewhere project it
// is linked to. Everything is read from files; no git or somewhere process is
// spawned, so resolving a few hundred sessions costs a few hundred small reads.
type repository struct {
	topLevel  string
	github    string
	somewhere string
}

func inspectRepository(cwd string) repository {
	result := repository{}
	gitDir, workTree := findGit(cwd)
	if workTree != "" {
		result.topLevel = canonical(workTree)
		result.github = githubRemote(gitDir)
	}
	base := result.topLevel
	if base == "" {
		base = cwd
	}
	result.somewhere = somewhereProject(base)
	return result
}

// findGit walks up from cwd looking for a .git entry. A directory is the
// repository itself. A file is a worktree pointer ("gitdir: <main>/.git/
// worktrees/<name>"); the main checkout is the parent of the common .git
// directory, which is what groups every worktree of one repository together.
// It returns the common git directory (where config lives) and the top-level.
func findGit(cwd string) (gitDir, topLevel string) {
	dir := cwd
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Lstat(candidate)
		if err == nil {
			if info.IsDir() {
				return candidate, dir
			}
			if pointed := readGitdirPointer(candidate, dir); pointed != "" {
				common := commonGitDir(pointed)
				return common, filepath.Dir(common)
			}
			return "", ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func readGitdirPointer(path, base string) string {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(encoded))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target)
}

// commonGitDir strips the worktree suffix: <repo>/.git/worktrees/<name> ->
// <repo>/.git. A pointer that is not a worktree (a submodule, say) is
// returned as is.
func commonGitDir(gitDir string) string {
	parts := strings.Split(gitDir, string(filepath.Separator))
	for index := len(parts) - 1; index >= 1; index-- {
		if parts[index-1] == "worktrees" && index >= 2 && parts[index-2] == ".git" {
			return strings.Join(parts[:index-1], string(filepath.Separator))
		}
	}
	// A "commondir" file inside a worktree git dir names the shared directory
	// when the layout is not the default one.
	if encoded, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(encoded))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		return filepath.Clean(common)
	}
	return gitDir
}

var githubRemotePattern = regexp.MustCompile(`github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)

// githubRemote reads [remote "origin"] url from the repository config and
// returns owner/repo when it points at github.com. Any other host yields "".
func githubRemote(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	file, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	inOrigin := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin || !strings.HasPrefix(line, "url") {
			continue
		}
		_, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if match := githubRemotePattern.FindStringSubmatch(strings.TrimSpace(value)); match != nil {
			return match[1] + "/" + match[2]
		}
	}
	return ""
}

// somewhereProject reads the Somewhere CLI's per-folder link, when present,
// and returns the project identifier it names. The file is the CLI's; only
// the id is taken from it.
func somewhereProject(base string) string {
	for _, candidate := range []string{
		filepath.Join(base, ".somewhere", "config.json"),
		filepath.Join(base, "somewhere.json"),
	} {
		encoded, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var parsed map[string]any
		if json.Unmarshal(encoded, &parsed) != nil {
			continue
		}
		for _, key := range []string{"projectId", "project_id", "id"} {
			if value, ok := parsed[key].(string); ok && value != "" {
				return value
			}
		}
		if nested, ok := parsed["project"].(map[string]any); ok {
			for _, key := range []string{"id", "projectId"} {
				if value, ok := nested[key].(string); ok && value != "" {
					return value
				}
			}
		}
	}
	return ""
}
