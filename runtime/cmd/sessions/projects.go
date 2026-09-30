package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type projectView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Implicit   bool     `json:"implicit"`
	Roots      []string `json:"roots"`
	GitHub     string   `json:"github,omitempty"`
	Somewhere  string   `json:"somewhere,omitempty"`
	Pinned     bool     `json:"pinned,omitempty"`
	SessionIDs []string `json:"session_ids"`
	Live       int      `json:"live"`
	NeedsInput int      `json:"needs_input"`
}

type projectRecord struct {
	ID        string   `json:"id,omitempty"`
	Name      string   `json:"name"`
	Roots     []string `json:"roots"`
	GitHub    string   `json:"github,omitempty"`
	Somewhere string   `json:"somewhere,omitempty"`
	Pinned    bool     `json:"pinned,omitempty"`
}

// cmdProjects lists the projects sessions are grouped under and names them.
//
//	sessions projects                        list stored and implicit projects
//	sessions projects name <folder> <name>   claim a folder (its git checkout and
//	                                         every worktree of it) under a name
//	sessions projects forget <id>            drop a stored project; sessions return
//	                                         to their folder's implicit project
func (a *app) cmdProjects(args []string) error {
	if len(args) == 0 {
		return a.listProjects()
	}
	switch args[0] {
	case "name":
		if len(args) != 3 {
			return fail(1, "usage: sessions projects name <folder> <name>")
		}
		return a.nameProject(args[1], args[2])
	case "forget":
		if len(args) != 2 {
			return fail(1, "usage: sessions projects forget <project-id>")
		}
		var out map[string]any
		if err := a.deleteJSON("/api/projects/"+escapeID(args[1]), &out); err != nil {
			return err
		}
		if a.wantJSON {
			return writeJSON(a.stdout, out, true)
		}
		_, err := io.WriteString(a.stdout, "forgot project "+args[1]+"\n")
		return err
	default:
		return fail(1, "usage: sessions projects [name <folder> <name> | forget <project-id>]")
	}
}

func (a *app) listProjects() error {
	var response struct {
		Projects []projectView `json:"projects"`
	}
	if err := a.getJSON("/api/projects", &response); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, response.Projects, true)
	}
	if len(response.Projects) == 0 {
		_, err := io.WriteString(a.stdout, "(no projects: start a session and its folder appears here)\n")
		return err
	}
	rows := [][]string{{"ID", "NAME", "KIND", "ROOT", "SESSIONS", "LIVE", "NEEDS-YOU"}}
	for _, item := range response.Projects {
		kind := "named"
		id := item.ID
		if item.Implicit {
			kind = "folder"
			id = "-"
		}
		if item.GitHub != "" {
			kind += " · github"
		}
		if item.Somewhere != "" {
			kind += " · somewhere"
		}
		root := "-"
		if len(item.Roots) > 0 {
			root = a.homeRelative(item.Roots[0])
			if len(item.Roots) > 1 {
				root += " +" + strconv.Itoa(len(item.Roots)-1)
			}
		}
		needs := "-"
		if item.NeedsInput > 0 {
			needs = strconv.Itoa(item.NeedsInput)
		}
		rows = append(rows, []string{
			id, item.Name, kind, root,
			strconv.Itoa(len(item.SessionIDs)), strconv.Itoa(item.Live), needs,
		})
	}
	if err := writePaddedRows(a.stdout, rows); err != nil {
		return err
	}
	_, err := io.WriteString(a.stdout, "\nname a folder's project with `sessions projects name <folder> <name>`\n")
	return err
}

func (a *app) nameProject(folder, name string) error {
	absolute, err := filepath.Abs(folder)
	if err != nil {
		return fail(1, "resolve %s: %v", folder, err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return fail(1, "%s is not a folder", absolute)
	}
	var suggestion projectRecord
	if err := a.getJSON("/api/projects/suggest?cwd="+escapeID(absolute), &suggestion); err != nil {
		return fail(2, "could not name this project: %s", err)
	}
	suggestion.Name = strings.TrimSpace(name)
	var saved projectRecord
	if err := a.putJSON("/api/projects", suggestion, &saved, 1); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, saved, true)
	}
	root := "-"
	if len(saved.Roots) > 0 {
		root = a.homeRelative(saved.Roots[0])
	}
	_, err = io.WriteString(a.stdout, "project "+saved.Name+" ("+saved.ID+") now covers "+root+" and its worktrees\n")
	return err
}

func (a *app) deleteJSON(path string, target any) error {
	response, err := a.api.request(context.Background(), http.MethodDelete, path, nil, 0)
	if err != nil {
		return fail(2, "%s → %s", path, err)
	}
	if response.status >= 400 {
		return apiReadFailure(path, response)
	}
	if target == nil || len(response.body) == 0 {
		return nil
	}
	return json.Unmarshal(response.body, target)
}
