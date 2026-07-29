package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	backupstore "github.com/somewhere-tech/sessions/runtime/internal/backup"
)

func TestBackupEnableStatusAndNow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokenPath := backupstore.SomewhereConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"auth":{"token":"smt_cli-fixture"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/backup/reload":
			_, _ = response.Write([]byte(`{"ok":true}`))
		case "/api/backup/now":
			_, _ = response.Write([]byte(`{"pushed_at":"2026-07-16T12:00:00Z","uploaded":2,"skipped":3,"session_count":5,"manifest_path":"sessions/test/manifest.json"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "backup", "enable", "--project", "cli-project", "--interval", "7m"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "cli-project") {
		t.Fatalf("enable code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	configPath := backupstore.ConfigPath(home)
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	encoded, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("smt_cli-fixture")) {
		t.Fatal("backup config contains token")
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backup", "status", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var status backupstore.Status
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.Project != "cli-project" || status.Interval != "7m0s" {
		t.Fatalf("status = %#v", status)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "backup", "now"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "2 uploaded, 3 unchanged, 5 sessions") {
		t.Fatalf("now code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestBackupEncryptedEnableStatusAndDecrypt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokenPath := backupstore.SomewhereConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"token":"smt_cli-encryption-fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/backup/reload" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "backup", "enable", "--project", "encrypted-project", "--encrypt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), "RECOVERY PHRASE:") ||
		!strings.Contains(stdout.String(), "WITHOUT IT YOUR BACKUPS ARE UNRECOVERABLE; WE CANNOT RESET IT") {
		t.Fatalf("enable code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	config, err := backupstore.LoadConfig(backupstore.ConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Encrypt {
		t.Fatalf("config = %#v", config)
	}
	keyPath := backupstore.KeyPath(home)
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key mode = %o, want 600", got)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backup", "status"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "encryption: on (key: ~/.config/sessions/backup.key)") {
		t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	key, err := backupstore.ReadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := backupstore.RecoveryPhrase(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("decrypted CLI fixture\n")
	payload, err := backupstore.Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(home, "transcript.jsonl.enc")
	outputPath := filepath.Join(home, "recovered.jsonl")
	if err := os.WriteFile(inputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backup", "decrypt", inputPath, "--out", outputPath, "--key-phrase", phrase}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("decrypt code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	decrypted, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}

	defaultInputPath := filepath.Join(home, "manifest.json.enc")
	if err := os.WriteFile(defaultInputPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backup", "decrypt", defaultInputPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("local-key decrypt code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	decrypted, err = os.ReadFile(strings.TrimSuffix(defaultInputPath, ".enc"))
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("local-key decrypted = %q, %v", decrypted, err)
	}

	wrongKey := bytes.Repeat([]byte{0xff}, 32)
	wrongPhrase, err := backupstore.RecoveryPhrase(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongOutput := filepath.Join(home, "wrong.jsonl")
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backup", "decrypt", inputPath, "--out", wrongOutput, "--key-phrase", wrongPhrase}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "wrong key or corrupted file") {
		t.Fatalf("wrong-key code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(wrongOutput); !os.IsNotExist(err) {
		t.Fatalf("wrong-key output exists: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "backup", "enable", "--project", "encrypted-project", "--encrypt"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "Reusing the existing key") {
		t.Fatalf("reenable code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
