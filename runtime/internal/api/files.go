package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const maxUploadBody = 25 * 1024 * 1024

var unsafeUploadName = regexp.MustCompile(`[^A-Za-z0-9_.\- ]`)

type directoryCandidate struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

type directoryEntry struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Hidden bool   `json:"hidden"`
}

func listDirectoryCandidates(sessions []state.SessionInfo) []directoryCandidate {
	home, err := os.UserHomeDir()
	if err != nil {
		return []directoryCandidate{}
	}
	common := []string{"Desktop", "Documents", "Downloads", "Code", "code", "projects", "Projects", "dev", "src", "work"}
	seen := make(map[string]bool)
	out := make([]directoryCandidate, 0, 50)
	push := func(path, kind string, check bool) {
		if seen[path] {
			return
		}
		if check {
			if _, err := os.Stat(path); err != nil {
				return
			}
		}
		seen[path] = true
		label := path
		if path == home {
			label = "~"
		} else if strings.HasPrefix(path, home+string(filepath.Separator)) {
			label = "~" + strings.TrimPrefix(path, home)
		}
		out = append(out, directoryCandidate{Path: path, Label: label, Kind: kind})
	}

	// Somewhere is the primary project surface for Sessions. Discover its
	// checked-out projects and managed worktrees before generic development
	// folders so starting useful work does not require filesystem knowledge.
	for _, rootName := range []string{"somewhere", "Somewhere"} {
		root := filepath.Join(home, rootName)
		if isProjectDirectory(root) {
			push(root, "somewhere", true)
		}
		for _, path := range listProjectChildren(root, 24) {
			push(path, "somewhere", true)
		}
		worktrees := filepath.Join(root, "wt")
		for _, path := range listProjectChildren(worktrees, 30) {
			push(path, "somewhere", true)
		}
	}

	// A folder already used by Sessions is safe to recommend without probing
	// or recursively reading it. This keeps real work on the selected machine
	// easy to return to, including projects stored outside conventional roots.
	recent := append([]state.SessionInfo(nil), sessions...)
	sort.SliceStable(recent, func(left, right int) bool {
		return directorySessionActivity(recent[left]) > directorySessionActivity(recent[right])
	})
	for _, session := range recent {
		path := filepath.Clean(strings.TrimSpace(session.Cwd))
		if path == "." || !pathWithinBase(path, home) {
			continue
		}
		kind := "project"
		if pathWithinSomewhere(path, home) {
			kind = "somewhere"
		}
		push(path, kind, false)
		if len(out) >= 40 {
			break
		}
	}

	// Desktop, Documents, and Downloads may be iCloud-backed on macOS. A
	// synchronous ReadDir there can wait on remote hydration and used to hold
	// the New Session launcher open indefinitely. Do not even stat protected
	// common folders or sweep the whole home directory here: those background
	// probes can trigger macOS privacy prompts. Expand only conventional local
	// development roots, then offer broad folders as explicit choices.
	for _, name := range []string{"Code", "code", "projects", "Projects", "dev", "src", "work"} {
		root := filepath.Join(home, name)
		if isProjectDirectory(root) {
			push(root, "project", true)
		}
		for _, path := range listProjectChildren(root, 15) {
			push(path, "project", true)
		}
		if len(out) >= 50 {
			break
		}
	}
	push(home, "home", false)
	for _, name := range common {
		push(filepath.Join(home, name), "common", false)
	}
	return out
}

func directorySessionActivity(session state.SessionInfo) int64 {
	if session.LastUserMessageAt != nil {
		return *session.LastUserMessageAt
	}
	return session.CreatedAt
}

func pathWithinSomewhere(path, home string) bool {
	for _, rootName := range []string{"somewhere", "Somewhere"} {
		if pathWithinBase(path, filepath.Join(home, rootName)) {
			return true
		}
	}
	return false
}

func listProjectChildren(parent string, maximum int) []string {
	return listProjectChildrenExcept(parent, maximum, nil)
}

func listProjectChildrenExcept(parent string, maximum int, skip map[string]struct{}) []string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return []string{}
	}
	projects := make([]string, 0, maximum)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, skipped := skip[entry.Name()]; skipped {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !isProjectDirectory(path) {
			continue
		}
		projects = append(projects, path)
		if len(projects) >= maximum {
			break
		}
	}
	sort.Strings(projects)
	return projects
}

func isProjectDirectory(path string) bool {
	for _, marker := range []string{".git", "package.json", "pyproject.toml", "Cargo.toml", "go.mod"} {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	return false
}

func (s *Server) handleFSList(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	home, err := os.UserHomeDir()
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	path := ""
	if values, present := request.URL.Query()["path"]; present && len(values) > 0 {
		path = values[0]
	} else {
		path = home
	}
	if !filepath.IsAbs(path) {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "path must be absolute"}, corsOrigin)
		return
	}
	canonical := canonicalPath(path)
	canonicalHome := canonicalPath(home)
	if !pathWithinBase(canonical, canonicalHome) {
		s.sendJSON(response, http.StatusForbidden, map[string]any{
			"error": "path outside home directory", "path": canonical,
		}, corsOrigin)
		return
	}

	root, err := os.OpenRoot(canonicalHome)
	if err != nil {
		s.sendFilesystemError(response, err, corsOrigin)
		return
	}
	defer root.Close()
	relative, err := filepath.Rel(canonicalHome, canonical)
	if err != nil {
		s.sendFilesystemError(response, err, corsOrigin)
		return
	}
	directory, err := root.Open(relative)
	if err != nil {
		s.sendFilesystemError(response, err, corsOrigin)
		return
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		s.sendFilesystemError(response, err, corsOrigin)
		return
	}
	if !info.IsDir() {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{
			"error": "not a directory", "path": canonical,
		}, corsOrigin)
		return
	}
	children, err := directory.ReadDir(-1)
	if err != nil {
		s.sendFilesystemError(response, err, corsOrigin)
		return
	}
	entries := make([]directoryEntry, 0, len(children))
	for _, child := range children {
		kind := "other"
		// ReadDir already reports the entry type without opening the child.
		// Do not Lstat or resolve every home-folder entry just to draw a
		// picker: protected folders and cloud-drive symlinks can prompt for
		// macOS access even when the user never chose them. An explicitly
		// typed path still passes through canonicalPath and the normal
		// within-home check before it is opened.
		switch entryType := child.Type(); {
		case entryType&os.ModeSymlink != 0:
			kind = "symlink"
		case child.IsDir():
			kind = "dir"
		case entryType.IsRegular():
			kind = "file"
		}
		entries = append(entries, directoryEntry{
			Name: child.Name(), Kind: kind, Hidden: strings.HasPrefix(child.Name(), "."),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		iDir := entries[i].Kind == "dir"
		jDir := entries[j].Kind == "dir"
		if iDir != jDir {
			return iDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	var parent any
	if canonical != canonicalHome {
		parent = filepath.Dir(canonical)
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"path": canonical, "parent": parent, "entries": entries,
	}, corsOrigin)
}

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	absolute = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	current := absolute
	tail := make([]string, 0, 4)
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return absolute
		}
		tail = append([]string{filepath.Base(current)}, tail...)
		current = parent
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, tail...)
			return filepath.Join(parts...)
		}
	}
}

func pathWithinBase(path, base string) bool {
	relative, err := filepath.Rel(base, path)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Server) sendFilesystemError(response http.ResponseWriter, err error, corsOrigin string) {
	status := http.StatusInternalServerError
	code := filesystemErrorCode(err)
	switch code {
	case "ENOENT":
		status = http.StatusNotFound
	case "EACCES":
		status = http.StatusForbidden
	}
	body := map[string]any{"error": err.Error()}
	if code != "" {
		body["code"] = code
	}
	s.sendJSON(response, status, body, corsOrigin)
}

func filesystemErrorCode(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ""
	}
	switch errno {
	case syscall.EACCES:
		return "EACCES"
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EPERM:
		return "EPERM"
	case syscall.ENOTDIR:
		return "ENOTDIR"
	case syscall.ELOOP:
		return "ELOOP"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG"
	case syscall.EIO:
		return "EIO"
	case syscall.EMFILE:
		return "EMFILE"
	case syscall.ENFILE:
		return "ENFILE"
	default:
		return ""
	}
}

func (s *Server) handleUpload(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	filename := request.Header.Get("X-Sessions-Filename")
	if filename == "" {
		filename = "file"
	}
	safeBase := unsafeUploadName.ReplaceAllString(nodeBaseName(filename), "_")
	if len(safeBase) > 96 {
		safeBase = safeBase[:96]
	}
	extension := nodeExtension(safeBase)
	stem := strings.TrimSuffix(safeBase, extension)
	if stem == "" {
		stem = "file"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	uploadsDir := filepath.Join(home, ".local", "state", "sessions", "uploads")
	canonicalHome := canonicalPath(home)
	if !pathWithinBase(canonicalPath(uploadsDir), canonicalHome) {
		s.sendJSON(response, http.StatusForbidden, map[string]any{"error": "upload directory outside home"}, corsOrigin)
		return
	}
	root, err := os.OpenRoot(canonicalHome)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	defer root.Close()
	relativeUploads := filepath.Join(".local", "state", "sessions", "uploads")
	if err := root.MkdirAll(relativeUploads, 0o700); err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxUploadBody+1))
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if len(body) > maxUploadBody {
		s.sendJSON(response, http.StatusRequestEntityTooLarge, map[string]any{
			"error": "file too large", "max": maxUploadBody,
		}, corsOrigin)
		return
	}
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	outName := stem + "-" + hex.EncodeToString(random) + extension
	outPath := filepath.Join(uploadsDir, outName)
	if err := root.WriteFile(filepath.Join(relativeUploads, outName), body, 0o600); err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"path": outPath, "size": len(body)}, corsOrigin)
}

func nodeBaseName(path string) string {
	path = strings.TrimRight(path, string(filepath.Separator))
	if path == "" {
		return ""
	}
	if index := strings.LastIndexByte(path, byte(filepath.Separator)); index >= 0 {
		return path[index+1:]
	}
	return path
}

func nodeExtension(base string) string {
	if base == "." || base == ".." {
		return ""
	}
	index := strings.LastIndexByte(base, '.')
	if index <= 0 {
		return ""
	}
	return base[index:]
}
