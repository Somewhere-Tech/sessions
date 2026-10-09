package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestartPreviewReportsChangedSavedAccountWithoutMutation(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	path := filepath.Join(source.ConfigDir, "projects")
	entries, _ := os.ReadDir(path)
	transcript := filepath.Join(path, entries[0].Name(), doubleOpenConversation+".jsonl")
	appendFile, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = appendFile.WriteString(`{"type":"bridge-session","bridgeSessionId":"cse_old","ownerAccountUuid":"original"}` + "\n")
	appendFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(source.ConfigDir, ".claude.json")
	if err := os.WriteFile(login, []byte(`{"oauthAccount":{"accountUuid":"different","emailAddress":"new@example.test"},"secret":"must-not-leak"}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/restart/preview", strings.NewReader(`{"sourceSessionId":"`+source.ID+`"}`), "127.0.0.1:1", nil)
	var result restartPreview
	decodeBody(t, response, &result)
	if response.Code != 200 || !result.AccountChanged || result.SavedLoginEmail != "new@example.test" || result.RemoteURL != "https://claude.ai/code/session_old" || result.Warning == "" {
		t.Fatalf("preview=%d %+v", response.Code, result)
	}
	if strings.Contains(response.Body.String(), "must-not-leak") || strings.Contains(response.Body.String(), "original") && strings.Contains(response.Body.String(), "accountUuid") {
		t.Fatal("preview leaked provider data")
	}
	live, _ := daemon.manager.Get(source.ID)
	if live.HasExited() || len(daemon.launcher.Launches) != 1 {
		t.Fatal("preview changed a runtime")
	}
	if _, err := os.Stat(daemon.handler.restartReceiptPath(restartOperationID(source.ID))); !os.IsNotExist(err) {
		t.Fatalf("preview wrote a receipt: %v", err)
	}
}

func TestRecordedClaudeBridgeIsBoundedAndUsesLatestOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	data := `{"type":"bridge-session","bridgeSessionId":"cse_old","ownerAccountUuid":"old"}` + "\n" + strings.Repeat("x", (1<<20)+128) + "\n" + `{"type":"bridge-session","bridgeSessionId":"cse_new","ownerAccountUuid":"new"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	owner, url := recordedClaudeBridge(path)
	if owner != "new" || url != "https://claude.ai/code/session_new" {
		t.Fatalf("owner=%q url=%q", owner, url)
	}
	if err := os.WriteFile(path, []byte(`{"type":"bridge-session","bridgeSessionId":"cse_bad?token=secret","ownerAccountUuid":"new"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, url = recordedClaudeBridge(path)
	if url != "" {
		t.Fatal("unsafe bridge id became a link")
	}
}

func TestRestartPreviewUnknownMetadataIsNotAccountMatch(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	result := claudeRestartPreview(source)
	if result.AccountChanged || result.RemoteURL != "" || !strings.Contains(result.Warning, "could not be compared") {
		t.Fatalf("unknown=%+v", result)
	}
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/restart/preview", strings.NewReader(`{"sourceSessionId":"missing"}`), "127.0.0.1:1", nil)
	if response.Code != 404 {
		t.Fatalf("missing source=%d", response.Code)
	}
}

func TestRestartPreviewRequiresAuthentication(t *testing.T) {
	daemon := restartTestDaemon(t)
	source := restartTestSource(t, daemon)
	response := serve(t, daemon.handler, http.MethodPost, "/api/recovery/restart/preview", strings.NewReader(`{"sourceSessionId":"`+source.ID+`"}`), "192.168.1.44:1234", nil)
	if response.Code != 401 {
		t.Fatalf("unauthenticated preview=%d", response.Code)
	}
}
