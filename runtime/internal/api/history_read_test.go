package api

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func TestConversationReadHTTPContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	const id = "11111111-2222-4333-8444-555555555555"
	path := writeClaudeHistoryFixture(t, daemon, home, id, "reader fixture", claudeTranscriptLines("first", "second"))
	endpoint := "/api/history/" + id + "/read"
	response := serve(t, daemon.handler, http.MethodGet, endpoint+"?limit=1", nil, "127.0.0.1:4321", nil)
	var page integrations.ReadResponse
	decodeBody(t, response, &page)
	if response.Code != 200 || len(page.Messages) != 1 || page.Messages[0].Text != "first" || !page.HasMore {
		t.Fatalf("first=%s", response.Body)
	}
	nextURL := endpoint + "?cursor=" + url.QueryEscape(page.NextCursor)
	second := serve(t, daemon.handler, http.MethodGet, nextURL, nil, "127.0.0.1:4321", nil)
	retry := serve(t, daemon.handler, http.MethodGet, nextURL, nil, "127.0.0.1:4321", nil)
	if second.Code != 200 || !bytes.Equal(second.Body.Bytes(), retry.Body.Bytes()) || !bytes.Contains(second.Body.Bytes(), []byte("second")) {
		t.Fatalf("retry=%s / %s", second.Body, retry.Body)
	}
	for _, suffix := range []string{"?limit=0", "?limit=101", "?cursor=garbage"} {
		bad := serve(t, daemon.handler, http.MethodGet, endpoint+suffix, nil, "127.0.0.1:4321", nil)
		if bad.Code != 400 {
			t.Fatalf("%s=%d %s", suffix, bad.Code, bad.Body)
		}
	}
	unauthorized := serve(t, daemon.handler, http.MethodGet, endpoint, nil, "198.51.100.20:4321", nil)
	if unauthorized.Code != 401 {
		t.Fatalf("unauthorized=%d", unauthorized.Code)
	}
	if err := os.WriteFile(path, claudeTranscriptLines("rewritten"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := serve(t, daemon.handler, http.MethodGet, nextURL, nil, "127.0.0.1:4321", nil)
	if changed.Code != 409 || !bytes.Contains(changed.Body.Bytes(), []byte("HISTORY_CHANGED")) {
		t.Fatalf("changed=%d %s", changed.Code, changed.Body)
	}
}
