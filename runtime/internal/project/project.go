// Package project groups sessions by the work they belong to.
//
// A project is a named record with one or more root folders and, optionally,
// the GitHub repository or Somewhere project it corresponds to. A session
// finds its project by its working directory: the git top-level first (a
// worktree resolves to its main checkout, so every worktree of one repository
// lands in one project), then a match against project roots. A folder no
// project claims is still shown, as an implicit project named after the
// folder, so the inbox is organized on day one and naming is a promotion
// rather than a prerequisite.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Project is the stored record. Roots are absolute folders; a session whose
// resolved top-level sits at or under a root belongs here.
type Project struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Roots     []string `json:"roots"`
	GitHub    string   `json:"github,omitempty"`
	Somewhere string   `json:"somewhere,omitempty"`
	Pinned    bool     `json:"pinned,omitempty"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// Resolution is what a working directory maps to. Implicit is true when no
// stored project claims the folder; the ID is then derived from the top-level
// so the same folder resolves to the same implicit project every time.
type Resolution struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Implicit  bool   `json:"implicit"`
	TopLevel  string `json:"top_level"`
	GitHub    string `json:"github,omitempty"`
	Somewhere string `json:"somewhere,omitempty"`
}

type projectFile struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
}

const fileVersion = 1

// Store owns projects.json and answers folder lookups. Reads are served from
// memory; writes are atomic and serialized.
type Store struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	projects []Project
	loaded   bool
}

func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now}
}

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	encoded, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read projects: %w", err)
	}
	var parsed projectFile
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return fmt.Errorf("parse projects %s: %w", s.path, err)
	}
	s.projects = parsed.Projects
	s.loaded = true
	return nil
}

func (s *Store) save(projects []Project) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create projects directory: %w", err)
	}
	encoded, err := json.MarshalIndent(projectFile{Version: fileVersion, Projects: projects}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode projects: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".projects-*")
	if err != nil {
		return fmt.Errorf("create temporary projects file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write projects: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace projects: %w", err)
	}
	return nil
}

// List returns stored projects, pinned first, then by most recent update.
func (s *Store) List() ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := make([]Project, len(s.projects))
	copy(out, s.projects)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// Upsert creates or updates a project. An empty ID creates one; a root that
// another project already claims is rejected so a folder never belongs to two
// projects. Roots are cleaned and de-duplicated.
func (s *Store) Upsert(input Project) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Project{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return Project{}, errors.New("a project needs a name")
	}
	if len(input.Name) > 120 {
		return Project{}, errors.New("a project name must be 120 characters or fewer")
	}
	roots := make([]string, 0, len(input.Roots))
	seen := map[string]bool{}
	for _, root := range input.Roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || root == "." || !filepath.IsAbs(root) {
			return Project{}, fmt.Errorf("project root %q must be an absolute folder", root)
		}
		root = canonical(root)
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return Project{}, errors.New("a project needs at least one root folder")
	}
	for _, other := range s.projects {
		if other.ID == input.ID {
			continue
		}
		for _, root := range roots {
			for _, claimed := range other.Roots {
				if root == claimed {
					return Project{}, fmt.Errorf("%s already belongs to project %q", root, other.Name)
				}
			}
		}
	}
	now := s.now().UnixMilli()
	if input.ID == "" {
		input.ID = newID(roots[0], now)
		input.CreatedAt = now
	}
	input.Roots = roots
	input.UpdatedAt = now
	projects := append([]Project(nil), s.projects...)
	replaced := false
	for index, existing := range projects {
		if existing.ID == input.ID {
			if input.CreatedAt == 0 {
				input.CreatedAt = existing.CreatedAt
			}
			projects[index] = input
			replaced = true
			break
		}
	}
	if !replaced {
		if input.CreatedAt == 0 {
			input.CreatedAt = now
		}
		projects = append(projects, input)
	}
	if err := s.save(projects); err != nil {
		return Project{}, err
	}
	s.projects = projects
	return input, nil
}

// Delete forgets a stored project. Its sessions return to being an implicit
// project; nothing about them is touched.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	kept := make([]Project, 0, len(s.projects))
	found := false
	for _, project := range s.projects {
		if project.ID == id {
			found = true
			continue
		}
		kept = append(kept, project)
	}
	if !found {
		return fmt.Errorf("no project %s", id)
	}
	if err := s.save(kept); err != nil {
		return err
	}
	s.projects = kept
	return nil
}

// Resolve maps a working directory to its project. The top-level is the git
// main checkout when the folder is inside a repository (worktrees included),
// otherwise the folder itself. A stored project whose root contains the
// top-level wins; otherwise the answer is implicit.
func (s *Store) Resolve(cwd string) (Resolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Resolution{}, err
	}
	return s.resolveLocked(cwd), nil
}

func (s *Store) resolveLocked(cwd string) Resolution {
	cwd = canonical(cwd)
	repo := inspectRepository(cwd)
	top := cwd
	if repo.topLevel != "" {
		top = repo.topLevel
	}
	best := Project{}
	bestLen := -1
	for _, project := range s.projects {
		for _, root := range project.Roots {
			if root == top || strings.HasPrefix(top, root+string(filepath.Separator)) {
				if len(root) > bestLen {
					best, bestLen = project, len(root)
				}
			}
		}
	}
	if bestLen >= 0 {
		return Resolution{ProjectID: best.ID, Name: best.Name, TopLevel: top, GitHub: best.GitHub, Somewhere: best.Somewhere}
	}
	name := repo.github
	if name == "" {
		name = filepath.Base(top)
	}
	return Resolution{
		ProjectID: "implicit:" + implicitID(top), Name: name, Implicit: true,
		TopLevel: top, GitHub: repo.github, Somewhere: repo.somewhere,
	}
}

// Suggest is what "Name this project" starts from for an implicit project:
// the folder's top-level as the single root, the GitHub repo or folder name
// as the name, and whatever the folder identifies about itself.
func (s *Store) Suggest(cwd string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Project{}, err
	}
	resolution := s.resolveLocked(cwd)
	if !resolution.Implicit {
		// The folder is already claimed: hand back that project so naming
		// it again renames in place instead of colliding with itself.
		for _, project := range s.projects {
			if project.ID == resolution.ProjectID {
				return project, nil
			}
		}
	}
	return Project{Name: resolution.Name, Roots: []string{resolution.TopLevel}, GitHub: resolution.GitHub, Somewhere: resolution.Somewhere}, nil
}

func newID(root string, now int64) string {
	return "p_" + implicitID(root)[:12] + "_" + fmt.Sprintf("%x", now%0xffffff)
}

func implicitID(top string) string {
	sum := sha256.Sum256([]byte(top))
	return hex.EncodeToString(sum[:8])
}

// canonical resolves symlinks so one folder has one identity. On macOS /tmp is
// a link to /private/tmp and a worktree's git pointer records the resolved
// form while a session's cwd may not; without this the same checkout split
// into two projects. An unresolvable path is cleaned and used as given.
func canonical(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
