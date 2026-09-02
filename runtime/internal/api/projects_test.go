package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/project"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestProjectViewsGroupSessionsAndKeepStoredProjectsVisible(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/somewhere-tech/sessions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	store := project.NewStore(filepath.Join(root, "projects.json"), nil)
	named, err := store.Upsert(project.Project{Name: "Somewhere", Roots: []string{filepath.Join(root, "somewhere")}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := state.SessionInfo{ID: "b", Cwd: filepath.Join(repo, "runtime"), IdleReason: state.IdleReasonNeedsInput}
	views, err := projectViews(store, []state.SessionInfo{
		{ID: "a", Cwd: repo}, blocked,
		{ID: "c", Cwd: plain, Exited: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]projectView{}
	for _, view := range views {
		byName[view.Name] = view
	}
	if view := byName["somewhere-tech/sessions"]; !view.Implicit || len(view.SessionIDs) != 2 || view.Live != 2 || view.NeedsInput != 1 {
		t.Fatalf("repo project = %#v", view)
	}
	if view := byName["notes"]; !view.Implicit || len(view.SessionIDs) != 1 || view.Live != 0 {
		t.Fatalf("plain folder project = %#v", view)
	}
	if view := byName["Somewhere"]; view.Implicit || view.ID != named.ID || len(view.SessionIDs) != 0 {
		t.Fatalf("stored project without sessions = %#v", view)
	}
	if views[0].Implicit {
		t.Fatalf("stored projects should sort ahead of implicit ones: %#v", views)
	}
}

func TestProjectsRouteCreatesAndForgets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	folder := filepath.Join(home, "work")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	suggested := serve(t, daemon.handler, http.MethodGet, "/api/projects/suggest?cwd="+folder, nil, "127.0.0.1:4321", nil)
	if suggested.Code != http.StatusOK || !strings.Contains(suggested.Body.String(), `"name":"work"`) {
		t.Fatalf("implicit suggest status=%d body=%s", suggested.Code, suggested.Body.String())
	}
	body := `{"name":"Work","roots":["` + folder + `"]}`
	created := serve(t, daemon.handler, http.MethodPut, "/api/projects", strings.NewReader(body), "127.0.0.1:4321", http.Header{"Content-Type": []string{"application/json"}})
	if created.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", created.Code, created.Body.String())
	}
	var saved project.Project
	decodeBody(t, created, &saved)
	if saved.ID == "" || saved.Name != "Work" {
		t.Fatalf("saved = %#v", saved)
	}
	listed := serve(t, daemon.handler, http.MethodGet, "/api/projects", nil, "127.0.0.1:4321", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"name":"Work"`) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	// Once claimed, suggest hands back the stored project (with its id) so a
	// second `name` renames rather than colliding.
	claimed := serve(t, daemon.handler, http.MethodGet, "/api/projects/suggest?cwd="+folder, nil, "127.0.0.1:4321", nil)
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"id":"`+saved.ID+`"`) {
		t.Fatalf("claimed suggest status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	forgotten := serve(t, daemon.handler, http.MethodDelete, "/api/projects/"+saved.ID, nil, "127.0.0.1:4321", nil)
	if forgotten.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", forgotten.Code, forgotten.Body.String())
	}
	if again := serve(t, daemon.handler, http.MethodDelete, "/api/projects/"+saved.ID, nil, "127.0.0.1:4321", nil); again.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d", again.Code)
	}
}

func TestProjectsSuggestRouteReportsProjectFileError(t *testing.T) {
	daemon := newTestDaemon(t)
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.handler.projects = project.NewStore(path, nil)

	response := serve(t, daemon.handler, http.MethodGet, "/api/projects/suggest?cwd="+t.TempDir(), nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "Sessions could not suggest this project: parse projects") {
		t.Fatalf("suggest status=%d body=%s", response.Code, response.Body.String())
	}
}
