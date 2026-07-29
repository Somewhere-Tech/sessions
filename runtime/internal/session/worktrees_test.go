package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestWorktreeCreateUsesNameBaseProvenanceAndCollisionSuffix(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "project")
	writeAndCommit(t, repo, "release.txt", "release\n", "release base")
	gitTest(t, repo, "branch", "release")
	writeAndCommit(t, repo, "main.txt", "main\n", "main advances")

	manager, launcher, store := newWorktreeTestManager(t, root)
	first, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "Fix Parser", Worktree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt", "fix-parser")
	if first.Cwd != wantPath || first.WorktreePath != wantPath || first.Branch != "sessions/fix-parser" ||
		first.Base != "main" || first.SourceRepo != repo {
		t.Fatalf("created worktree provenance = %#v", first)
	}
	if branch := strings.TrimSpace(gitTest(t, first.Cwd, "branch", "--show-current")); branch != first.Branch {
		t.Fatalf("worktree branch = %q, want %q", branch, first.Branch)
	}
	if len(launcher.Launches) != 1 || launcher.Launches[0].Info.Cwd != wantPath {
		t.Fatalf("runner launches = %#v", launcher.Launches)
	}

	events, err := store.Events(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := ledger.Fold(events)
	if len(states) != 1 || states[0].WorktreePath != wantPath || states[0].Branch != first.Branch ||
		states[0].Base != "main" || states[0].SourceRepo != repo {
		t.Fatalf("folded worktree provenance = %#v", states)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"worktree_path": wantPath, "branch": "sessions/fix-parser", "base": "main", "source_repo": repo,
	} {
		if payload[key] != want {
			t.Fatalf("created payload[%q] = %#v, want %q; payload=%#v", key, payload[key], want, payload)
		}
	}

	override, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "Release Fix", Worktree: true, Base: "release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if override.Base != "release" || override.Branch != "sessions/release-fix" {
		t.Fatalf("base override provenance = %#v", override)
	}
	if got, want := strings.TrimSpace(gitTest(t, override.Cwd, "rev-parse", "HEAD")), strings.TrimSpace(gitTest(t, repo, "rev-parse", "release")); got != want {
		t.Fatalf("base override HEAD = %q, want release %q", got, want)
	}

	collision, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "Fix Parser", Worktree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if collision.Cwd == first.Cwd || collision.Branch == first.Branch ||
		!strings.HasPrefix(collision.Branch, "sessions/fix-parser-") || !strings.HasPrefix(filepath.Base(collision.Cwd), "fix-parser-") {
		t.Fatalf("collision did not receive a short suffix: first=%#v collision=%#v", first, collision)
	}
}

func TestWorktreeCreateTeachingErrorsAreAtomic(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "atomic")
	manager, launcher, store := newWorktreeTestManager(t, root)

	assertAtomicFailure := func(name, cwd, base, contains string) {
		t.Helper()
		beforeLaunches := len(launcher.Launches)
		beforeEvents, err := store.Events(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		_, createErr := manager.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "/bin/sh", Cwd: cwd, Name: name, Worktree: true, Base: base,
		})
		if createErr == nil || !strings.Contains(createErr.Error(), contains) {
			t.Fatalf("Create() error = %v, want %q", createErr, contains)
		}
		afterEvents, err := store.Events(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(launcher.Launches) != beforeLaunches || len(afterEvents) != len(beforeEvents) {
			t.Fatalf("failed create mutated sessions: launches %d->%d events %d->%d",
				beforeLaunches, len(launcher.Launches), len(beforeEvents), len(afterEvents))
		}
	}

	assertAtomicFailure("No Repo", filepath.Join(root, "not-a-repo"), "", "inside a Git repository")
	assertAtomicFailure("Bad Base", repo, "missing-base", "does not name a commit")
	if _, err := os.Stat(filepath.Join(root, "atomic-wt", "bad-base")); !os.IsNotExist(err) {
		t.Fatalf("invalid base left worktree path behind: %v", err)
	}

	bare := filepath.Join(root, "bare.git")
	runGitTest(t, "", "clone", "--bare", repo, bare)
	assertAtomicFailure("Bare", bare, "main", "non-bare Git checkout")

	shallow := filepath.Join(root, "shallow")
	runGitTest(t, "", "clone", "--depth", "1", "file://"+repo, shallow)
	assertAtomicFailure("Shallow", shallow, "", "does not use shallow repositories")
}

type failingWorktreeBoundary struct {
	delegate ledger.BoundaryWriter
}

func (f failingWorktreeBoundary) RecordCreated(context.Context, ledger.Created) error {
	return errors.New("ledger create unavailable")
}

func (f failingWorktreeBoundary) RecordProviderRebound(ctx context.Context, value ledger.ProviderRebound) error {
	return f.delegate.RecordProviderRebound(ctx, value)
}

func (f failingWorktreeBoundary) RecordUserKill(ctx context.Context, value ledger.UserKill) error {
	return f.delegate.RecordUserKill(ctx, value)
}

func TestWorktreeCreateRollsBackIfWriteAheadLedgerBoundaryFails(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "rollback")
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(filepath.Join(root, "daemon")), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries:   failingWorktreeBoundary{delegate: store.Boundaries()},
		Observations: store.Observations(), LedgerReader: store, Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)

	_, createErr := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: "Rollback Me", Worktree: true,
	})
	if createErr == nil || !strings.Contains(createErr.Error(), "ledger create unavailable") {
		t.Fatalf("Create() error = %v", createErr)
	}
	path := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt", "rollback-me")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed ledger boundary left worktree path: %v", err)
	}
	if _, err := gitOutput(context.Background(), repo, "show-ref", "--verify", "--quiet", "refs/heads/sessions/rollback-me"); err == nil {
		t.Fatal("failed ledger boundary left worktree branch")
	}
	events, err := store.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || len(launcher.Launches) != 0 {
		t.Fatalf("failed ledger boundary mutated session state: events=%d launches=%d", len(events), len(launcher.Launches))
	}
}

func TestWorktreesListReportsCleanDirtyAndMergedState(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "listing")
	manager, _, _ := newWorktreeTestManager(t, root)

	clean := createWorktreeSession(t, manager, repo, "Clean List")
	dirty := createWorktreeSession(t, manager, repo, "Dirty List")
	if err := os.WriteFile(filepath.Join(dirty.Cwd, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ahead := createWorktreeSession(t, manager, repo, "Ahead List")
	writeAndCommit(t, ahead.Cwd, "ahead.txt", "ahead\n", "unmerged work")

	listed, err := manager.Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bySession := make(map[string]WorktreeStatus, len(listed))
	for _, item := range listed {
		bySession[item.SessionID] = item
	}
	if got := bySession[clean.ID]; got.TreeState != "clean" || got.Dirty || !got.MergedIntoBase || got.SessionState != "live" {
		t.Fatalf("clean status = %#v", got)
	}
	if got := bySession[dirty.ID]; got.TreeState != "DIRTY" || !got.Dirty || !got.MergedIntoBase {
		t.Fatalf("dirty status = %#v", got)
	}
	if got := bySession[ahead.ID]; got.TreeState != "clean" || got.Dirty || got.MergedIntoBase {
		t.Fatalf("unmerged status = %#v", got)
	}
}

func TestWorktreesCleanOnlyRemovesDeadCleanMergedAndDryRunDoesNothing(t *testing.T) {
	root := t.TempDir()
	repo := initWorktreeTestRepo(t, root, "cleaning")
	manager, launcher, _ := newWorktreeTestManager(t, root)

	eligible := createWorktreeSession(t, manager, repo, "Eligible")
	dirty := createWorktreeSession(t, manager, repo, "Dirty")
	if err := os.WriteFile(filepath.Join(dirty.Cwd, "dirty.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unmerged := createWorktreeSession(t, manager, repo, "Unmerged")
	writeAndCommit(t, unmerged.Cwd, "commit.txt", "not merged\n", "not merged")
	live := createWorktreeSession(t, manager, repo, "Live")
	for _, id := range []string{eligible.ID, dirty.ID, unmerged.ID} {
		exitWorktreeSession(t, manager, launcher, id)
	}

	dryRun, err := manager.CleanWorktrees(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	dryBySession := cleanResultsBySession(dryRun)
	if dryBySession[eligible.ID].Action != "would-remove" {
		t.Fatalf("eligible dry-run = %#v", dryBySession[eligible.ID])
	}
	for _, item := range []state.SessionInfo{eligible, dirty, unmerged, live} {
		if _, err := os.Stat(item.Cwd); err != nil {
			t.Fatalf("dry-run mutated %s: %v", item.Cwd, err)
		}
	}

	results, err := manager.CleanWorktrees(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	bySession := cleanResultsBySession(results)
	if bySession[eligible.ID].Action != "removed" {
		t.Fatalf("eligible clean = %#v", bySession[eligible.ID])
	}
	if _, err := os.Stat(eligible.Cwd); !os.IsNotExist(err) {
		t.Fatalf("eligible path still exists: %v", err)
	}
	if _, err := gitOutput(context.Background(), repo, "show-ref", "--verify", "--quiet", "refs/heads/"+eligible.Branch); err == nil {
		t.Fatalf("eligible branch %s still exists", eligible.Branch)
	}
	if got := bySession[dirty.ID]; got.Action != "skipped" || got.Reason != "worktree is DIRTY" {
		t.Fatalf("dirty clean result = %#v", got)
	}
	if got := bySession[unmerged.ID]; got.Action != "skipped" || !strings.Contains(got.Reason, "not fully merged") {
		t.Fatalf("unmerged clean result = %#v", got)
	}
	if got := bySession[live.ID]; got.Action != "skipped" || got.Reason != "session is live" {
		t.Fatalf("live clean result = %#v", got)
	}
	for _, item := range []state.SessionInfo{dirty, unmerged, live} {
		if _, err := os.Stat(item.Cwd); err != nil {
			t.Fatalf("unsafe clean removed %s: %v", item.Cwd, err)
		}
	}
}

func initWorktreeTestRepo(t *testing.T, root, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "init", "-b", "main")
	gitTest(t, repo, "config", "user.name", "Sessions Test")
	gitTest(t, repo, "config", "user.email", "sessions@example.test")
	writeAndCommit(t, repo, "README.md", "fixture\n", "initial")
	return strings.TrimSpace(gitTest(t, repo, "rev-parse", "--show-toplevel"))
}

func writeAndCommit(t *testing.T, repo, name, contents, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", name)
	gitTest(t, repo, "commit", "-m", message)
}

func newWorktreeTestManager(t *testing.T, root string) (*Manager, *prototest.Launcher, *ledger.Store) {
	t.Helper()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(filepath.Join(root, "daemon")), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)
	return manager, launcher, store
}

func createWorktreeSession(t *testing.T, manager *Manager, repo, name string) state.SessionInfo {
	t.Helper()
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: repo, Name: name, Worktree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func exitWorktreeSession(t *testing.T, manager *Manager, launcher *prototest.Launcher, id string) {
	t.Helper()
	code := 0
	launcher.Runner(id).Emit(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &code}})
	awaitCondition(t, func() bool {
		session, ok := manager.Get(id)
		if !ok || !session.Info().Exited {
			return false
		}
		listed, err := manager.Worktrees(context.Background())
		if err != nil {
			return false
		}
		for _, worktree := range listed {
			if worktree.SessionID == id {
				return worktree.SessionState == "exited"
			}
		}
		return false
	})
}

func cleanResultsBySession(results []WorktreeCleanResult) map[string]WorktreeCleanResult {
	indexed := make(map[string]WorktreeCleanResult, len(results))
	for _, result := range results {
		indexed[result.SessionID] = result
	}
	return indexed
}

func gitTest(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	return runGitTest(t, cwd, args...)
}

func runGitTest(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	if cwd != "" {
		command.Dir = cwd
	}
	encoded, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), cwd, err, encoded)
	}
	return string(encoded)
}
