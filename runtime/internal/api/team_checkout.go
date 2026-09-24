package api

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type checkoutWarning struct {
	Path     string   `json:"path,omitempty"`
	Dirty    *bool    `json:"dirty,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Detail   string   `json:"detail"`
}

// This is advisory, not ownership enforcement. We cannot know which external
// process may write here, or prevent one agent from resetting another's work.
func attachCheckoutWarnings(ctx context.Context, listing *teamListing, infos []state.SessionInfo) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	roots, groups := checkoutGroups(ctx, infos)
	checked := make(map[string]*checkoutWarning)
	for i := range listing.Members {
		member := &listing.Members[i]
		root := roots[member.Cwd]
		if root == "" || member.Exited {
			continue
		}
		if len(groups[root]) < 2 {
			continue
		}
		warning, seen := checked[root]
		if !seen {
			warning = inspectSharedCheckout(ctx, root, groups[root])
			checked[root] = warning
		}
		member.Checkout = warning
	}
}

func checkoutGroups(ctx context.Context, infos []state.SessionInfo) (map[string]string, map[string][]string) {
	roots := make(map[string]string)
	groups := make(map[string][]string)
	for _, info := range infos {
		if info.Exited || info.RunnerGone || info.Cwd == "" {
			continue
		}
		root, found := roots[info.Cwd]
		if !found {
			if len(roots) >= 64 || ctx.Err() != nil {
				continue
			}
			output, err := checkoutGit(ctx, info.Cwd, "rev-parse", "--show-toplevel")
			if err == nil {
				root = strings.TrimSpace(output)
				if resolved, err := filepath.EvalSymlinks(root); err == nil {
					root = resolved
				}
			}
			roots[info.Cwd] = root
		}
		if root != "" {
			groups[root] = append(groups[root], info.ID)
		}
	}
	for _, ids := range groups {
		sort.Strings(ids)
	}
	return roots, groups
}

func inspectSharedCheckout(ctx context.Context, root string, ids []string) *checkoutWarning {
	warning := &checkoutWarning{Path: root, Sessions: ids}
	output, err := checkoutGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		warning.Detail = "Multiple sessions share this checkout. Its dirty state could not be checked; coordinate writes or use separate worktrees."
		return warning
	}
	dirty := strings.TrimSpace(output) != ""
	warning.Dirty = &dirty
	if dirty {
		warning.Detail = "Multiple sessions share this checkout with uncommitted changes. Coordinate writes or use separate worktrees; do not reset another session's work."
	} else {
		warning.Detail = "Multiple sessions share this checkout. It is currently clean, but their future edits can collide."
	}
	return warning
}

// Cap output as well as runtime. Git status is read-only, needs no credential,
// and must not refresh the index or contact a remote merely to show a warning.
func checkoutGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-C", cwd}, args...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var output cappedGitOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	err := cmd.Run()
	return output.String(), err
}

type cappedGitOutput struct{ strings.Builder }

func (output *cappedGitOutput) Write(data []byte) (int, error) {
	length := len(data)
	remaining := 64*1024 - output.Len()
	if remaining > 0 {
		_, _ = output.Builder.Write(data[:min(remaining, length)])
	}
	return length, nil
}
