package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repository with a GitHub origin, a worktree of it, and a plain folder.
func fixtureTree(t *testing.T) (repo, worktree, plain string) {
	t.Helper()
	root := canonical(t.TempDir())
	repo = filepath.Join(root, "sessions")
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(repo, ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:somewhere-tech/sessions.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	worktree = filepath.Join(root, "sessions-lanes")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "sessions-lanes")+"\n")
	writeFile(t, filepath.Join(repo, ".git", "worktrees", "sessions-lanes", "HEAD"), "ref: refs/heads/lanes\n")
	plain = filepath.Join(root, "notes")
	writeFile(t, filepath.Join(plain, "todo.md"), "x\n")
	return repo, worktree, plain
}

func TestResolveGroupsWorktreesUnderTheMainCheckout(t *testing.T) {
	repo, worktree, plain := fixtureTree(t)
	store := NewStore(filepath.Join(t.TempDir(), "projects.json"), nil)

	main, err := store.Resolve(filepath.Join(repo, "runtime", "internal"))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := store.Resolve(filepath.Join(worktree, "frontend"))
	if err != nil {
		t.Fatal(err)
	}
	if !main.Implicit || main.TopLevel != repo || main.GitHub != "somewhere-tech/sessions" || main.Name != "somewhere-tech/sessions" {
		t.Fatalf("main checkout resolution = %#v", main)
	}
	if wt.ProjectID != main.ProjectID || wt.TopLevel != repo {
		t.Fatalf("worktree did not fold into its main checkout: %#v vs %#v", wt, main)
	}
	other, err := store.Resolve(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !other.Implicit || other.Name != "notes" || other.TopLevel != plain || other.GitHub != "" {
		t.Fatalf("plain folder resolution = %#v", other)
	}
	if other.ProjectID == main.ProjectID {
		t.Fatal("unrelated folder shares an implicit project id")
	}
}

func TestNamedProjectClaimsFoldersAndRejectsDoubleClaims(t *testing.T) {
	repo, worktree, plain := fixtureTree(t)
	path := filepath.Join(t.TempDir(), "projects.json")
	clock := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	store := NewStore(path, func() time.Time { return clock })

	suggestion, err := store.Suggest(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.Name != "somewhere-tech/sessions" || len(suggestion.Roots) != 1 || suggestion.Roots[0] != repo {
		t.Fatalf("suggestion = %#v", suggestion)
	}
	suggestion.Name = "Sessions"
	created, err := store.Upsert(suggestion)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.CreatedAt != clock.UnixMilli() {
		t.Fatalf("created = %#v", created)
	}
	resolved, _ := store.Resolve(filepath.Join(worktree, "src"))
	if resolved.Implicit || resolved.ProjectID != created.ID || resolved.Name != "Sessions" {
		t.Fatalf("worktree after naming = %#v", resolved)
	}
	if _, err := store.Upsert(Project{Name: "Dup", Roots: []string{repo}}); err == nil {
		t.Fatal("a second project claimed the same root")
	}
	if _, err := store.Upsert(Project{Name: "", Roots: []string{plain}}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := store.Upsert(Project{Name: "Rel", Roots: []string{"relative/path"}}); err == nil {
		t.Fatal("relative root accepted")
	}

	// The file round-trips through a fresh store.
	again := NewStore(path, nil)
	listed, err := again.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].GitHub != "somewhere-tech/sessions" {
		t.Fatalf("reloaded = %#v", listed)
	}
	if err := again.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	back, _ := again.Resolve(repo)
	if !back.Implicit {
		t.Fatalf("deleted project still claims folder: %#v", back)
	}
}

func TestSuggestReturnsProjectFileLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, nil)
	if _, err := store.Suggest(t.TempDir()); err == nil {
		t.Fatal("suggestion hid an unreadable project file")
	}
}

func TestFailedSaveDoesNotChangeLoadedProjects(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projects.json")
	store := NewStore(path, nil)
	created, err := store.Upsert(Project{Name: "Before", Roots: []string{filepath.Join(root, "repo")}})
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blocker, "projects.json")

	updated := created
	updated.Name = "After"
	if _, err := store.Upsert(updated); err == nil {
		t.Fatal("update at an unwritable path succeeded")
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "Before" {
		t.Fatalf("failed update changed memory: %#v", listed)
	}

	if err := store.Delete(created.ID); err == nil {
		t.Fatal("delete at an unwritable path succeeded")
	}
	listed, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("failed delete changed memory: %#v", listed)
	}

	persisted, err := NewStore(path, nil).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Name != "Before" {
		t.Fatalf("failed writes changed disk: %#v", persisted)
	}
}

func TestSomewhereMarkerIsRead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".somewhere", "config.json"), `{"projectId":"proj_123","token":"never-read"}`)
	store := NewStore(filepath.Join(t.TempDir(), "projects.json"), nil)
	resolved, _ := store.Resolve(root)
	if resolved.Somewhere != "proj_123" {
		t.Fatalf("somewhere id = %q", resolved.Somewhere)
	}
}

// /tmp on macOS is a symlink to /private/tmp. A session started through the
// link and a worktree whose git pointer holds the resolved path must land in
// the same project.
func TestResolveCanonicalizesSymlinkedFolders(t *testing.T) {
	base := canonical(t.TempDir())
	real := filepath.Join(base, "real")
	writeFile(t, filepath.Join(real, "repo", ".git", "HEAD"), "ref: refs/heads/main\n")
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	store := NewStore(filepath.Join(t.TempDir(), "projects.json"), nil)
	viaLink, _ := store.Resolve(filepath.Join(link, "repo", "src"))
	viaReal, _ := store.Resolve(filepath.Join(real, "repo"))
	if viaLink.ProjectID != viaReal.ProjectID {
		t.Fatalf("symlinked path split the project: %#v vs %#v", viaLink, viaReal)
	}
	named, err := store.Upsert(Project{Name: "Repo", Roots: []string{filepath.Join(link, "repo")}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := store.Resolve(filepath.Join(real, "repo")); resolved.ProjectID != named.ID {
		t.Fatalf("root stored via link does not claim the real path: %#v", resolved)
	}
}
