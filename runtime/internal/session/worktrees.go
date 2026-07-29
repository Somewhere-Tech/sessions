package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type createdWorktree struct {
	Path       string
	Branch     string
	Base       string
	SourceRepo string
}

// WorktreeStatus is derived from immutable ledger provenance plus live Git
// state. Sessions never discovers arbitrary worktrees and therefore cannot
// accidentally adopt or clean work it did not create.
type WorktreeStatus struct {
	SessionID       string `json:"session"`
	SessionName     string `json:"session_name,omitempty"`
	WorktreePath    string `json:"worktree_path"`
	Branch          string `json:"branch"`
	Base            string `json:"base"`
	SourceRepo      string `json:"source_repo"`
	TreeState       string `json:"tree_state"`
	Dirty           bool   `json:"dirty"`
	MergedIntoBase  bool   `json:"merged_into_base"`
	SessionState    string `json:"session_state"`
	InspectionError string `json:"inspection_error,omitempty"`
	Exists          bool   `json:"exists"`
}

type WorktreeCleanResult struct {
	WorktreeStatus
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

func createGitWorktree(ctx context.Context, cwd, sessionName, requestedBase string) (createdWorktree, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return createdWorktree{}, errors.New("--worktree needs Git, but `git` is not available; install Git and retry")
	}
	resolvedCwd, err := filepath.Abs(cwd)
	if err != nil {
		return createdWorktree{}, fmt.Errorf("resolve worktree source cwd: %w", err)
	}
	bare, err := gitOutput(ctx, resolvedCwd, "rev-parse", "--is-bare-repository")
	if err == nil && strings.TrimSpace(bare) == "true" {
		return createdWorktree{}, errors.New("--worktree needs a non-bare Git checkout; pass --cwd for a working repository")
	}
	inside, insideErr := gitOutput(ctx, resolvedCwd, "rev-parse", "--is-inside-work-tree")
	if insideErr != nil || strings.TrimSpace(inside) != "true" {
		return createdWorktree{}, errors.New("--worktree needs --cwd (or $PWD) inside a Git repository; cd into a repository or pass --cwd and retry")
	}
	shallow, err := gitOutput(ctx, resolvedCwd, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return createdWorktree{}, fmt.Errorf("inspect Git repository depth: %w", err)
	}
	if strings.TrimSpace(shallow) == "true" {
		return createdWorktree{}, errors.New("--worktree does not use shallow repositories; run `git fetch --unshallow` (or use a full checkout) and retry")
	}
	sourceRepo, err := gitOutput(ctx, resolvedCwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return createdWorktree{}, fmt.Errorf("find Git repository root: %w", err)
	}
	sourceRepo = strings.TrimSpace(sourceRepo)

	base := strings.TrimSpace(requestedBase)
	if base == "" {
		base, err = gitOutput(ctx, sourceRepo, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil || strings.TrimSpace(base) == "" {
			return createdWorktree{}, errors.New("Git HEAD is detached; pass --base <branch-or-ref> for the new worktree")
		}
		base = strings.TrimSpace(base)
	}
	if _, err := gitOutput(ctx, sourceRepo, "rev-parse", "--verify", "--quiet", "--end-of-options", base+"^{commit}"); err != nil {
		return createdWorktree{}, fmt.Errorf("base ref %q does not name a commit; fetch or create it, then retry", base)
	}

	slug := sanitizeWorktreeName(sessionName)
	repoName := filepath.Base(sourceRepo)
	if repoName == "." || repoName == string(filepath.Separator) || repoName == "" {
		repoName = "repo"
	}
	worktreeRoot := filepath.Join(filepath.Dir(sourceRepo), repoName+"-wt")
	branch, path, err := availableWorktreeIdentity(ctx, sourceRepo, worktreeRoot, slug)
	if err != nil {
		return createdWorktree{}, err
	}
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return createdWorktree{}, fmt.Errorf("create worktree parent %s: %w", worktreeRoot, err)
	}
	if _, err := gitOutput(ctx, sourceRepo, "worktree", "add", "-b", branch, "--", path, base); err != nil {
		worktree := createdWorktree{Path: path, Branch: branch, Base: base, SourceRepo: sourceRepo}
		if rollbackErr := rollbackFailedGitWorktree(ctx, worktree); rollbackErr != nil {
			return createdWorktree{}, fmt.Errorf("create Git worktree at %s: %w; partial work was preserved because safe rollback was refused: %v",
				path, err, rollbackErr)
		}
		return createdWorktree{}, fmt.Errorf("create Git worktree at %s: %w; resolve the Git error and retry", path, err)
	}
	return createdWorktree{Path: path, Branch: branch, Base: base, SourceRepo: sourceRepo}, nil
}

func availableWorktreeIdentity(ctx context.Context, sourceRepo, root, slug string) (string, string, error) {
	for attempt := 0; attempt < 20; attempt++ {
		candidate := slug
		if attempt > 0 {
			suffix, err := worktreeSuffix()
			if err != nil {
				return "", "", fmt.Errorf("generate worktree collision suffix: %w", err)
			}
			candidate += "-" + suffix
		}
		branch := "sessions/" + candidate
		path := filepath.Join(root, candidate)
		branchExists, err := localBranchExists(ctx, sourceRepo, branch)
		if err != nil {
			return "", "", err
		}
		_, pathErr := os.Lstat(path)
		pathExists := pathErr == nil
		if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect worktree path %s: %w", path, pathErr)
		}
		if !branchExists && !pathExists {
			return branch, path, nil
		}
	}
	return "", "", errors.New("could not find an unused worktree branch and directory after 20 attempts; choose a different --name")
}

func localBranchExists(ctx context.Context, sourceRepo, branch string) (bool, error) {
	_, err := gitOutput(ctx, sourceRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check Git branch %s: %w", branch, err)
}

func sanitizeWorktreeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var result strings.Builder
	separator := false
	for _, value := range name {
		if (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') {
			if separator && result.Len() > 0 {
				result.WriteByte('-')
			}
			separator = false
			result.WriteRune(value)
			continue
		}
		separator = true
	}
	slug := strings.Trim(result.String(), "-")
	if slug == "" {
		slug = "session"
	}
	if len(slug) > 48 {
		slug = strings.TrimRight(slug[:48], "-")
	}
	return slug
}

func worktreeSuffix() (string, error) {
	value := make([]byte, 3)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", cwd}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	encoded, err := command.CombinedOutput()
	if err == nil {
		return string(encoded), nil
	}
	detail := strings.TrimSpace(string(encoded))
	if detail == "" {
		return "", err
	}
	return "", fmt.Errorf("%w: %s", err, detail)
}

func rollbackCreatedGitWorktree(ctx context.Context, worktree createdWorktree) error {
	branch, err := gitOutput(ctx, worktree.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) != worktree.Branch {
		return errors.New("worktree branch changed")
	}
	status, err := gitOutput(ctx, worktree.Path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return fmt.Errorf("inspect rollback worktree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("worktree is DIRTY")
	}
	branchHead, err := gitOutput(ctx, worktree.SourceRepo, "rev-parse", "--verify", "refs/heads/"+worktree.Branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("inspect rollback branch: %w", err)
	}
	baseHead, err := gitOutput(ctx, worktree.SourceRepo, "rev-parse", "--verify", worktree.Base+"^{commit}")
	if err != nil {
		return fmt.Errorf("inspect rollback base: %w", err)
	}
	branchHead = strings.TrimSpace(branchHead)
	if branchHead != strings.TrimSpace(baseHead) {
		return errors.New("worktree branch advanced after creation")
	}
	if _, err := gitOutput(ctx, worktree.SourceRepo, "worktree", "remove", "--", worktree.Path); err != nil {
		return fmt.Errorf("remove rollback worktree: %w", err)
	}
	return deleteUnadvancedWorktreeBranch(ctx, worktree, branchHead)
}

func rollbackFailedGitWorktree(ctx context.Context, worktree createdWorktree) error {
	_, pathErr := os.Stat(worktree.Path)
	if pathErr == nil {
		return rollbackCreatedGitWorktree(ctx, worktree)
	}
	if !errors.Is(pathErr, os.ErrNotExist) {
		return fmt.Errorf("inspect partial worktree: %w", pathErr)
	}
	exists, err := localBranchExists(ctx, worktree.SourceRepo, worktree.Branch)
	if err != nil || !exists {
		return err
	}
	branchHead, err := gitOutput(ctx, worktree.SourceRepo, "rev-parse", "--verify", "refs/heads/"+worktree.Branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("inspect partial branch: %w", err)
	}
	baseHead, err := gitOutput(ctx, worktree.SourceRepo, "rev-parse", "--verify", worktree.Base+"^{commit}")
	if err != nil || strings.TrimSpace(branchHead) != strings.TrimSpace(baseHead) {
		return errors.New("partial worktree branch advanced after creation")
	}
	return deleteUnadvancedWorktreeBranch(ctx, worktree, strings.TrimSpace(branchHead))
}

func deleteUnadvancedWorktreeBranch(ctx context.Context, worktree createdWorktree, branchHead string) error {
	if _, err := gitOutput(ctx, worktree.SourceRepo, "branch", "-d", "--", worktree.Branch); err == nil {
		return nil
	}
	// A base override may not be the source checkout's HEAD, which makes
	// branch -d refuse even though the branch still points exactly at base.
	// update-ref's expected-old value keeps this fallback race-safe.
	if _, err := gitOutput(ctx, worktree.SourceRepo, "update-ref", "-d", "refs/heads/"+worktree.Branch, branchHead); err != nil {
		return fmt.Errorf("delete rollback branch: %w", err)
	}
	return nil
}

func (m *Manager) Worktrees(ctx context.Context) ([]WorktreeStatus, error) {
	states, err := m.ledgerStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read worktree provenance: %w", err)
	}
	result := make([]WorktreeStatus, 0)
	for _, lane := range states {
		if lane.WorktreePath == "" {
			continue
		}
		status := WorktreeStatus{
			SessionID: lane.LaneID, SessionName: lane.Name,
			WorktreePath: lane.WorktreePath, Branch: lane.Branch, Base: lane.Base, SourceRepo: lane.SourceRepo,
			SessionState: "live", TreeState: "missing",
		}
		// Kill intent is written before process termination and therefore is
		// not evidence that a session is dead. Cleanup requires an observed
		// runner exit or successful reap, and a live registry entry still wins.
		closed := lane.RunnerExited || lane.Reaped
		if live, ok := m.registry.Get(lane.LaneID); ok && !live.Info().Exited {
			closed = false
		}
		if closed {
			status.SessionState = "exited"
		}
		inspectWorktree(ctx, &status)
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SessionID != result[j].SessionID {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].WorktreePath < result[j].WorktreePath
	})
	return result, nil
}

func inspectWorktree(ctx context.Context, status *WorktreeStatus) {
	info, err := os.Stat(status.WorktreePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status.InspectionError = err.Error()
			status.TreeState = "error"
		}
		return
	}
	if !info.IsDir() {
		status.InspectionError = "worktree path is not a directory"
		status.TreeState = "error"
		return
	}
	status.Exists = true
	branch, err := gitOutput(ctx, status.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		status.InspectionError = "read worktree branch: " + err.Error()
		status.TreeState = "error"
		return
	}
	if current := strings.TrimSpace(branch); current != status.Branch {
		status.InspectionError = fmt.Sprintf("worktree branch changed to %q", current)
		status.TreeState = "error"
		return
	}
	porcelain, err := gitOutput(ctx, status.WorktreePath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		status.InspectionError = "inspect worktree changes: " + err.Error()
		status.TreeState = "error"
		return
	}
	status.Dirty = strings.TrimSpace(porcelain) != ""
	if status.Dirty {
		status.TreeState = "DIRTY"
	} else {
		status.TreeState = "clean"
	}
	command := exec.CommandContext(ctx, "git", "-C", status.SourceRepo, "merge-base", "--is-ancestor", "refs/heads/"+status.Branch, status.Base)
	encoded, mergeErr := command.CombinedOutput()
	if mergeErr == nil {
		status.MergedIntoBase = true
		return
	}
	var exit *exec.ExitError
	if errors.As(mergeErr, &exit) && exit.ExitCode() == 1 {
		return
	}
	detail := strings.TrimSpace(string(encoded))
	if detail != "" {
		status.InspectionError = "check merge into base: " + detail
	} else {
		status.InspectionError = "check merge into base: " + mergeErr.Error()
	}
	status.TreeState = "error"
}

func (m *Manager) CleanWorktrees(ctx context.Context, dryRun bool) ([]WorktreeCleanResult, error) {
	worktrees, err := m.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]WorktreeCleanResult, 0, len(worktrees))
	for _, worktree := range worktrees {
		result := WorktreeCleanResult{WorktreeStatus: worktree, Action: "skipped"}
		switch {
		case worktree.SessionState != "exited":
			result.Reason = "session is live"
		case worktree.InspectionError != "":
			result.Reason = worktree.InspectionError
		case !worktree.Exists:
			result.Reason = "worktree path is missing"
		case worktree.Dirty:
			result.Reason = "worktree is DIRTY"
		case !worktree.MergedIntoBase:
			result.Reason = "branch is not fully merged into base " + worktree.Base
		case dryRun:
			result.Action = "would-remove"
		default:
			fresh := worktree
			fresh.Dirty = false
			fresh.MergedIntoBase = false
			fresh.InspectionError = ""
			inspectWorktree(ctx, &fresh)
			if fresh.InspectionError != "" {
				result.Reason = "final safety check: " + fresh.InspectionError
				break
			}
			if fresh.Dirty {
				result.Reason = "final safety check: worktree is DIRTY"
				break
			}
			if !fresh.MergedIntoBase {
				result.Reason = "final safety check: branch is not fully merged into base " + fresh.Base
				break
			}
			branchHead, err := gitOutput(ctx, fresh.SourceRepo, "rev-parse", "--verify", "refs/heads/"+fresh.Branch+"^{commit}")
			if err != nil {
				result.Reason = "final safety check: cannot resolve branch: " + err.Error()
				break
			}
			branchHead = strings.TrimSpace(branchHead)
			if _, err := gitOutput(ctx, worktree.SourceRepo, "worktree", "remove", "--", worktree.WorktreePath); err != nil {
				result.Reason = "git worktree remove refused: " + err.Error()
				break
			}
			if _, err := gitOutput(ctx, worktree.SourceRepo, "branch", "-d", "--", worktree.Branch); err != nil {
				// branch -d compares against the source checkout's HEAD when
				// --base names a different ref. The explicit ancestry check above
				// is the safety proof; expected-old makes deletion race-safe.
				if _, updateErr := gitOutput(ctx, worktree.SourceRepo, "update-ref", "-d", "refs/heads/"+worktree.Branch, branchHead); updateErr != nil {
					result.Action = "removed-worktree"
					result.Reason = "worktree removed, but safe branch deletion was refused: " + updateErr.Error()
					break
				}
			}
			result.Action = "removed"
		}
		results = append(results, result)
	}
	return results, nil
}
