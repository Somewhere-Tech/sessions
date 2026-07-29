package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func TestEncryptRoundTripRecoveryPhraseAndWrongKey(t *testing.T) {
	key := make([]byte, keySize)
	for index := range key {
		key[index] = byte(index + 1)
	}
	plaintext := []byte("fixture plaintext that must survive the round trip")
	payload, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, plaintext) {
		t.Fatal("encrypted payload contains plaintext")
	}
	decrypted, err := Decrypt(key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}

	phrase, err := RecoveryPhrase(key)
	if err != nil {
		t.Fatal(err)
	}
	groups := strings.Fields(phrase)
	if len(groups) != recoveryGroupCount {
		t.Fatalf("recovery phrase groups = %d, want %d", len(groups), recoveryGroupCount)
	}
	for _, group := range groups {
		if len(group) != recoveryGroupLength {
			t.Fatalf("recovery group %q has length %d", group, len(group))
		}
	}
	recovered, err := KeyFromRecoveryPhrase(strings.ToLower(phrase))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovered, key) {
		t.Fatal("recovery phrase did not reconstruct the key")
	}
	decrypted, err = Decrypt(recovered, payload)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("recovery decrypt = %q, %v", decrypted, err)
	}

	wrongKey := bytes.Repeat([]byte{0xff}, keySize)
	if _, err := Decrypt(wrongKey, payload); !errors.Is(err, ErrWrongKeyOrCorruptedFile) {
		t.Fatalf("wrong-key error = %v", err)
	}
}

func TestLoadOrCreateKeyUsesPrivateModesAndReusesKey(t *testing.T) {
	home := t.TempDir()
	path := KeyPath(home)
	first, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(first) != keySize {
		t.Fatalf("created=%v key length=%d", created, len(first))
	}
	keyInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key mode = %o, want 600", got)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("key directory mode = %o, want 700", got)
	}
	second, created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || !bytes.Equal(second, first) {
		t.Fatal("existing key was not reused")
	}
}

func TestEnablingEncryptionRepushesEncryptedTranscriptAndManifest(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	projectsDir := filepath.Join(root, "claude-projects")
	cwd := filepath.Join(root, "worktree")
	for _, directory := range []string{runnerDir, projectsDir, cwd} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	marker := []byte("PRIVATE-FIXTURE-MARKER")
	conversation := []byte("{\"type\":\"user\",\"message\":\"PRIVATE-FIXTURE-MARKER\"}\n")
	conversationPath := filepath.Join(projectsDir, watch.EncodeClaudeCWD(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conversationPath, conversation, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(root, "somewhere.json")
	if err := os.WriteFile(tokenPath, []byte(`{"token":"`+fixtureToken+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	keyPath := filepath.Join(root, "backup.key")
	if err := SaveConfig(configPath, Config{
		Enabled: true, Project: "fixture-project", TokenPath: tokenPath,
		Interval: "15m", Cache: make(map[string]Fingerprint),
	}); err != nil {
		t.Fatal(err)
	}

	type upload struct {
		path        string
		contentType string
		body        []byte
	}
	var mu sync.Mutex
	var uploads []upload
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		uploads = append(uploads, upload{
			path: request.URL.Path, contentType: request.Header.Get("Content-Type"), body: body,
		})
		mu.Unlock()
		response.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	pusher := NewPusher(Options{
		ConfigPath: configPath, KeyPath: keyPath, RunnerStateDir: runnerDir,
		ClaudeProjectsDir: projectsDir, Machine: "fixture-mac",
		APIBase: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	live := []state.SessionInfo{{
		ID: id, Name: "private fixture session", Cmd: "claude", Args: []string{"--session-id", id},
		Cwd: cwd, Tool: state.ToolClaude, CreatedAt: now.Add(-time.Hour).UnixMilli(),
		LastDataAt: now.Add(-time.Minute).UnixMilli(),
	}}
	if _, err := pusher.Push(t.Context(), live); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Cache) != 1 {
		t.Fatalf("plaintext cache size = %d, want 1", len(config.Cache))
	}

	config, setup, err := EnableWithEncryption(
		configPath, tokenPath, keyPath, "fixture-project", 15*time.Minute, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Encrypt || len(config.Cache) != 0 || setup.Reused || setup.RecoveryPhrase == "" {
		t.Fatalf("encrypted enable config=%#v setup=%#v", config, setup)
	}
	result, err := pusher.Push(t.Context(), live)
	if err != nil {
		t.Fatal(err)
	}
	if result.Uploaded != 1 || result.Skipped != 0 || !strings.HasSuffix(result.ManifestPath, "manifest.json.enc") {
		t.Fatalf("encrypted result = %#v", result)
	}

	key, err := ReadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotUploads := append([]upload(nil), uploads...)
	mu.Unlock()
	if len(gotUploads) != 4 {
		t.Fatalf("uploads = %d, want two plaintext then two encrypted", len(gotUploads))
	}
	encryptedTranscript := gotUploads[2]
	if !strings.HasSuffix(encryptedTranscript.path, id+".jsonl.enc") ||
		encryptedTranscript.contentType != "application/octet-stream" ||
		bytes.Contains(encryptedTranscript.body, marker) {
		t.Fatalf("encrypted transcript upload = %#v", encryptedTranscript)
	}
	decrypted, err := Decrypt(key, encryptedTranscript.body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, conversation) {
		t.Fatalf("decrypted transcript = %q, want %q", decrypted, conversation)
	}

	encryptedManifest := gotUploads[3]
	if !strings.HasSuffix(encryptedManifest.path, "manifest.json.enc") ||
		encryptedManifest.contentType != "application/octet-stream" ||
		bytes.Contains(encryptedManifest.body, marker) ||
		bytes.Contains(encryptedManifest.body, []byte("private fixture session")) {
		t.Fatalf("encrypted manifest upload = %#v", encryptedManifest)
	}
	manifestBytes, err := Decrypt(key, encryptedManifest.body)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	entry, ok := manifest.Sessions[id]
	if !ok || entry.Name != "private fixture session" || !strings.HasSuffix(entry.Path, id+".jsonl.enc") {
		t.Fatalf("decrypted manifest = %#v", manifest)
	}

	config, setup, err = EnableWithEncryption(
		configPath, tokenPath, keyPath, "fixture-project", 15*time.Minute, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !setup.Reused || setup.RecoveryPhrase == "" || len(config.Cache) != 1 {
		t.Fatalf("reenable config=%#v setup=%#v", config, setup)
	}
}
