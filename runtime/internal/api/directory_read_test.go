package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type boundedDirectoryFixture struct {
	count     int
	err       error
	requested int
}

func (f *boundedDirectoryFixture) ReadDir(count int) ([]os.DirEntry, error) {
	f.requested = count
	return make([]os.DirEntry, f.count), f.err
}

func TestDirectoryReadHasFiniteBudgetAndDetectsTruncation(t *testing.T) {
	for _, test := range []struct {
		name      string
		count     int
		readErr   error
		wantErr   error
		wantCount int
	}{
		{name: "empty", readErr: io.EOF},
		{name: "complete", count: 12, readErr: io.EOF, wantCount: 12},
		{name: "exact boundary", count: maxDirectoryEntries, readErr: io.EOF, wantCount: maxDirectoryEntries},
		{name: "oversized", count: maxDirectoryEntries + 1, wantCount: maxDirectoryEntries, wantErr: errDirectoryTooLarge},
		{name: "unreadable", readErr: os.ErrPermission, wantErr: os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &boundedDirectoryFixture{count: test.count, err: test.readErr}
			entries, err := boundedDirectoryEntries(reader)
			if reader.requested != maxDirectoryEntries+1 {
				t.Fatalf("requested %d entries", reader.requested)
			}
			if !errors.Is(err, test.wantErr) || len(entries) != test.wantCount {
				t.Fatalf("count=%d err=%v, want count=%d err=%v", len(entries), err, test.wantCount, test.wantErr)
			}
		})
	}
}

func TestOversizedDirectoryResponseDoesNotClaimPartialSuccess(t *testing.T) {
	response := httptest.NewRecorder()
	server := &Server{}
	server.sendDirectoryReadError(response, errDirectoryTooLarge, "")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	decodeBody(t, response, &body)
	assertExactKeys(t, body, "error", "code", "maxEntries")
	if body["code"] != "DIRECTORY_TOO_LARGE" || body["maxEntries"] != float64(maxDirectoryEntries) {
		t.Fatalf("oversized response = %#v", body)
	}
}

func TestFSListEntryLimitIsEnforcedAtTheHTTPBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for index := range maxDirectoryEntries {
		path := filepath.Join(home, fmt.Sprintf("fixture-%05d", index))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{}
	response := httptest.NewRecorder()
	server.handleFSList(response, httptest.NewRequest(http.MethodGet, "/api/fs/list", nil), "")
	if response.Code != http.StatusOK {
		t.Fatalf("exact-boundary status=%d body=%s", response.Code, response.Body.String())
	}
	var listing struct {
		Entries []directoryEntry `json:"entries"`
	}
	decodeBody(t, response, &listing)
	if len(listing.Entries) != maxDirectoryEntries {
		t.Fatalf("listing count=%d", len(listing.Entries))
	}
	if err := os.WriteFile(filepath.Join(home, "one-too-many"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.handleFSList(response, httptest.NewRequest(http.MethodGet, "/api/fs/list", nil), "")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", response.Code, response.Body.String())
	}
}
