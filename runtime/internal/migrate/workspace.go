package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var checkpointPartPattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func PrepareWorkspace(ctx context.Context, cwd, sourceID string, options WorkspaceOptions) (Workspace, error) {
	if cwd == "" {
		return Workspace{}, errors.New("workspace cwd is required")
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return Workspace{}, fmt.Errorf("workspace cwd is not a directory: %s", cwd)
	}
	root, err := gitOutput(ctx, cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		var executableError *exec.Error
		if errors.As(err, &executableError) {
			return Workspace{}, fmt.Errorf("inspect workspace: %w", err)
		}
		return Workspace{Git: false}, nil
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve Git root: %w", err)
	}
	branch, err := gitOutput(ctx, root, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		return Workspace{}, errors.New("workspace is on a detached HEAD; move requires a branch")
	}
	upstream, err := gitOutput(ctx, root, nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || upstream == "" {
		return Workspace{}, fmt.Errorf("workspace branch %s has no upstream; push it and set an upstream before moving", branch)
	}
	remoteName, err := gitOutput(ctx, root, nil, "config", "--get", "branch."+branch+".remote")
	if err != nil || remoteName == "" || remoteName == "." {
		return Workspace{}, fmt.Errorf("workspace branch %s has no reachable remote", branch)
	}
	mergeRef, err := gitOutput(ctx, root, nil, "config", "--get", "branch."+branch+".merge")
	if err != nil || !strings.HasPrefix(mergeRef, "refs/heads/") {
		return Workspace{}, fmt.Errorf("workspace upstream %s is not a remote branch", upstream)
	}
	remoteURL, err := gitOutput(ctx, root, nil, "remote", "get-url", remoteName)
	if err != nil || remoteURL == "" {
		return Workspace{}, fmt.Errorf("read workspace remote %s: %w", remoteName, err)
	}
	head, err := gitOutput(ctx, root, nil, "rev-parse", "HEAD")
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace HEAD: %w", err)
	}
	status, err := gitOutput(ctx, root, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace status: %w", err)
	}
	dirty := status != ""
	remoteHead, err := remoteRevision(ctx, root, remoteName, mergeRef)
	if err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{
		Git: true, Root: root, RemoteName: remoteName, RemoteURL: remoteURL,
		Branch: branch, Revision: head, Dirty: dirty,
	}
	if !dirty && remoteHead == head {
		return workspace, nil
	}
	if !options.AllowDirty {
		if dirty {
			return Workspace{}, errors.New("workspace has uncommitted changes; commit and push them, or retry with --allow-dirty")
		}
		return Workspace{}, fmt.Errorf("workspace HEAD %s is not pushed to %s/%s", shortRevision(head), remoteName, branch)
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	part := strings.Trim(checkpointPartPattern.ReplaceAllString(sourceID, "-"), "-.")
	if part == "" {
		part = "session"
	}
	checkpoint := fmt.Sprintf("refs/sessions/checkpoints/%s-%d", part, now().UnixMilli())
	workspace.CheckpointRef = checkpoint
	if options.DryRun {
		return workspace, nil
	}
	revision := head
	if dirty {
		revision, err = snapshotWorktree(ctx, root, head, sourceID)
		if err != nil {
			return Workspace{}, err
		}
	}
	if _, err := gitOutput(ctx, root, nil, "update-ref", checkpoint, revision); err != nil {
		return Workspace{}, fmt.Errorf("create checkpoint ref %s: %w", checkpoint, err)
	}
	if _, err := gitOutput(ctx, root, nil, "push", remoteName, checkpoint+":"+checkpoint); err != nil {
		return Workspace{}, fmt.Errorf("push checkpoint ref %s: %w", checkpoint, err)
	}
	workspace.Revision = revision
	return workspace, nil
}

func remoteRevision(ctx context.Context, root, remote, ref string) (string, error) {
	output, err := gitOutput(ctx, root, nil, "ls-remote", "--exit-code", remote, ref)
	if err != nil {
		return "", fmt.Errorf("verify pushed workspace ref %s on %s: %w", ref, remote, err)
	}
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[1] != ref {
		return "", fmt.Errorf("verify pushed workspace ref %s on %s: unexpected response", ref, remote)
	}
	return fields[0], nil
}

func snapshotWorktree(ctx context.Context, root, head, sourceID string) (string, error) {
	index, err := os.CreateTemp("", "sessions-move-index-")
	if err != nil {
		return "", fmt.Errorf("create checkpoint index: %w", err)
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", fmt.Errorf("close checkpoint index: %w", err)
	}
	if err := os.Remove(indexPath); err != nil {
		return "", fmt.Errorf("prepare checkpoint index: %w", err)
	}
	defer os.Remove(indexPath)
	environment := append(os.Environ(),
		"GIT_INDEX_FILE="+indexPath,
		"GIT_AUTHOR_NAME=sessions move", "GIT_AUTHOR_EMAIL=sessions-move@localhost",
		"GIT_COMMITTER_NAME=sessions move", "GIT_COMMITTER_EMAIL=sessions-move@localhost",
	)
	if _, err := gitOutput(ctx, root, environment, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("checkpoint read tree: %w", err)
	}
	if _, err := gitOutput(ctx, root, environment, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("checkpoint stage snapshot: %w", err)
	}
	tree, err := gitOutput(ctx, root, environment, "write-tree")
	if err != nil {
		return "", fmt.Errorf("checkpoint write tree: %w", err)
	}
	commit, err := gitOutput(ctx, root, environment, "commit-tree", tree, "-p", head, "-m", "sessions move checkpoint for "+sourceID)
	if err != nil {
		return "", fmt.Errorf("checkpoint commit tree: %w", err)
	}
	return commit, nil
}

func EnsureWorkspace(ctx context.Context, cwd string, workspace Workspace) error {
	if !workspace.Git {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("target cwd must already exist for a non-Git move: %s", cwd)
		}
		return nil
	}
	if !filepath.IsAbs(workspace.Root) || workspace.Root == string(filepath.Separator) {
		return errors.New("target Git root must be an absolute non-root path")
	}
	relative, err := filepath.Rel(workspace.Root, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("target cwd is outside the Git root")
	}
	if workspace.RemoteURL == "" || workspace.Branch == "" || workspace.Revision == "" {
		return errors.New("target Git workspace metadata is incomplete")
	}
	if info, err := os.Stat(workspace.Root); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("target Git root is not a directory: %s", workspace.Root)
		}
		head, err := gitOutput(ctx, workspace.Root, nil, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("target path is not a Git checkout: %s", workspace.Root)
		}
		if head != workspace.Revision {
			return fmt.Errorf("target checkout is at %s, need %s; refusing to mutate an existing workspace", shortRevision(head), shortRevision(workspace.Revision))
		}
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return fmt.Errorf("target cwd is missing from checkout: %s", cwd)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect target Git root: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Root), 0o700); err != nil {
		return fmt.Errorf("create target Git parent: %w", err)
	}
	if _, err := gitOutput(ctx, filepath.Dir(workspace.Root), nil, "clone", "--no-checkout", workspace.RemoteURL, workspace.Root); err != nil {
		return fmt.Errorf("clone target workspace: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(workspace.Root)
		}
	}()
	if workspace.CheckpointRef != "" {
		if _, err := gitOutput(ctx, workspace.Root, nil, "fetch", "origin", workspace.CheckpointRef); err != nil {
			return fmt.Errorf("fetch target checkpoint: %w", err)
		}
		if _, err := gitOutput(ctx, workspace.Root, nil, "checkout", "--detach", workspace.Revision); err != nil {
			return fmt.Errorf("checkout target checkpoint: %w", err)
		}
	} else {
		remoteRef := "refs/heads/" + workspace.Branch + ":refs/remotes/origin/" + workspace.Branch
		if _, err := gitOutput(ctx, workspace.Root, nil, "fetch", "origin", remoteRef); err != nil {
			return fmt.Errorf("fetch target branch: %w", err)
		}
		if _, err := gitOutput(ctx, workspace.Root, nil, "checkout", "-B", workspace.Branch, workspace.Revision); err != nil {
			return fmt.Errorf("checkout target branch: %w", err)
		}
		_, _ = gitOutput(ctx, workspace.Root, nil, "branch", "--set-upstream-to=origin/"+workspace.Branch, workspace.Branch)
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("target cwd is missing from cloned checkout: %s", cwd)
	}
	cleanup = false
	return nil
}

func gitOutput(ctx context.Context, dir string, environment []string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", dir}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	if environment != nil {
		command.Env = environment
	}
	encoded, err := command.CombinedOutput()
	output := strings.TrimSpace(string(encoded))
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), output)
		}
		return "", err
	}
	return output, nil
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func DisplayRemote(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User(parsed.User.Username())
	return parsed.String()
}
