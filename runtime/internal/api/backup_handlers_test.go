package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/backup"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestBackupRoutesUseInjectedDirectPushService(t *testing.T) {
	daemon := newTestDaemon(t)
	root := t.TempDir()
	tokenPath := filepath.Join(root, "somewhere.json")
	if err := os.WriteFile(tokenPath, []byte(`{"token":"smt_api-fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	if err := backup.SaveConfig(configPath, backup.Config{
		Enabled: true, Project: "api-fixture", TokenPath: tokenPath, Interval: "1h",
	}); err != nil {
		t.Fatal(err)
	}
	var putPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		putPath = request.URL.Path
		response.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	daemon.handler.backups = backup.NewService(backup.Options{
		ConfigPath: configPath, RunnerStateDir: filepath.Join(root, "runners"),
		Machine: "api-mac", APIBase: upstream.URL, HTTPClient: upstream.Client(),
	}, func() []state.SessionInfo { return nil })
	defer daemon.handler.backups.Close()

	statusResponse := serve(t, daemon.handler, http.MethodGet, "/api/backup/status", nil, "127.0.0.1:1234", nil)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var status backup.Status
	decodeBody(t, statusResponse, &status)
	if !status.Enabled || status.Project != "api-fixture" {
		t.Fatalf("status = %#v", status)
	}

	nowResponse := serve(t, daemon.handler, http.MethodPost, "/api/backup/now", nil, "127.0.0.1:1234", nil)
	if nowResponse.Code != http.StatusOK {
		t.Fatalf("now code=%d body=%s", nowResponse.Code, nowResponse.Body.String())
	}
	if putPath != "/v1/fs/api-fixture/sessions/api-mac/manifest.json" {
		t.Fatalf("put path = %q", putPath)
	}
	var result backup.Result
	if err := json.Unmarshal(nowResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SessionCount != 0 || result.ManifestPath == "" {
		t.Fatalf("result = %#v", result)
	}
}
