package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

const maxDirectoryEntries = 10_000

var errDirectoryTooLarge = errors.New("directory contains too many entries")

type directoryReader interface {
	ReadDir(int) ([]os.DirEntry, error)
}

// Read one extra entry to distinguish a complete listing at the boundary from
// truncation. Never return a partial picker listing as a complete success.
func boundedDirectoryEntries(directory directoryReader) ([]os.DirEntry, error) {
	entries, err := directory.ReadDir(maxDirectoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxDirectoryEntries {
		return entries[:maxDirectoryEntries], errDirectoryTooLarge
	}
	return entries, nil
}

func (s *Server) sendDirectoryReadError(response http.ResponseWriter, err error, corsOrigin string) {
	if errors.Is(err, errDirectoryTooLarge) {
		s.sendJSON(response, http.StatusRequestEntityTooLarge, map[string]any{
			"error": "This folder has too many entries to browse. Enter the full path of the folder you want to use instead.",
			"code":  "DIRECTORY_TOO_LARGE", "maxEntries": maxDirectoryEntries,
		}, corsOrigin)
		return
	}
	s.sendFilesystemError(response, err, corsOrigin)
}

func directoryListingEntries(children []os.DirEntry) []directoryEntry {
	entries := make([]directoryEntry, 0, len(children))
	for _, child := range children {
		kind := "other"
		// Entry types require no child stat or cloud hydration/privacy prompt.
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
		iDir, jDir := entries[i].Kind == "dir", entries[j].Kind == "dir"
		if iDir != jDir {
			return iDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries
}
