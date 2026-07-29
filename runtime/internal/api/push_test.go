package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"golang.org/x/crypto/hkdf"
)

type pushTestSubscription struct {
	value      map[string]any
	privateKey []byte
	publicKey  []byte
	auth       []byte
}

type capturedPush struct {
	method string
	header http.Header
	body   []byte
}

func TestPushRoutesPersistAndRemoveSubscriptions(t *testing.T) {
	daemon := newTestDaemon(t)
	vapid := serve(t, daemon.handler, http.MethodGet, "/api/push/vapid", nil, "127.0.0.1:1", nil)
	if vapid.Code != http.StatusOK {
		t.Fatalf("vapid status=%d body=%s", vapid.Code, vapid.Body.String())
	}
	var vapidBody map[string]any
	decodeBody(t, vapid, &vapidBody)
	if vapidBody["publicKey"] == "" {
		t.Fatalf("vapid response = %#v", vapidBody)
	}

	invalid := serve(t, daemon.handler, http.MethodPost, "/api/push/subscribe", bytes.NewBufferString(`{"endpoint":""}`), "127.0.0.1:1", nil)
	if invalid.Code != http.StatusBadRequest || !bytes.Contains(invalid.Body.Bytes(), []byte("invalid push subscription")) {
		t.Fatalf("invalid subscription status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	subscription := map[string]any{
		"endpoint": "https://push.example/subscription", "expirationTime": nil,
		"keys": map[string]string{"p256dh": "public", "auth": "secret"},
	}
	encoded, _ := json.Marshal(subscription)
	added := serve(t, daemon.handler, http.MethodPost, "/api/push/subscribe", bytes.NewReader(encoded), "127.0.0.1:1", nil)
	if added.Code != http.StatusOK {
		t.Fatalf("subscribe status=%d body=%s", added.Code, added.Body.String())
	}
	path := filepath.Join(daemon.config.StateRoot, "push-subscriptions.json")
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(stored, []byte("push.example")) {
		t.Fatalf("stored subscriptions=%s err=%v", stored, err)
	}
	statusResponse := serve(t, daemon.handler, http.MethodGet, "/api/notify", nil, "127.0.0.1:1", nil)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("notify status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var notificationStatus notifyState
	decodeBody(t, statusResponse, &notificationStatus)
	if !notificationStatus.Subscribed || !notificationStatus.Notify.Done || !notificationStatus.Notify.Waiting || !notificationStatus.Notify.Lost {
		t.Fatalf("default notification status = %#v", notificationStatus)
	}
	removed := serve(t, daemon.handler, http.MethodPost, "/api/push/unsubscribe", bytes.NewBufferString(`{"endpoint":"https://push.example/subscription"}`), "127.0.0.1:1", nil)
	if removed.Code != http.StatusOK {
		t.Fatalf("unsubscribe status=%d body=%s", removed.Code, removed.Body.String())
	}
	stored, err = os.ReadFile(path)
	if err != nil || string(stored) != "[]" {
		t.Fatalf("subscriptions after remove=%q err=%v", stored, err)
	}
}

func TestStructuredTurnCompleteSendsExactlyOneEncryptedPush(t *testing.T) {
	mock, received := newCapturedPushServer(t)
	config, launcher, manager := newPushTestManagerWithTiming(t, 30*time.Millisecond, 60*time.Millisecond)
	manager.Push().SetHTTPClient(mock.Client())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(config.UserStateRoot, "push-subscriptions.json"), 0o600)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: config.DefaultCwd, Name: "encrypted turn", Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	const plaintextMarker = "AUTHPUSHIDLEPAYLOADMARKER"
	emitCodexTurn(launcher.Runner(created.ID), "turn-1", plaintextMarker+" finished cleanly.")

	var push capturedPush
	select {
	case push = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("mock endpoint did not receive the structured turn-complete notification")
	}
	if push.method != http.MethodPost {
		t.Fatalf("push method = %q", push.method)
	}
	if got := push.header.Get("Content-Encoding"); got != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if got := push.header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if push.header.Get("Authorization") == "" || push.header.Get("TTL") != "3600" || push.header.Get("Urgency") != "normal" {
		t.Fatalf("push headers = %#v", push.header)
	}
	if len(push.body) < 100 || bytes.Contains(push.body, []byte(plaintextMarker)) {
		t.Fatalf("push body is not an encrypted payload: bytes=%d plaintext=%t", len(push.body), bytes.Contains(push.body, []byte(plaintextMarker)))
	}
	payload := decryptPushPayload(t, push.body, subscription)
	if payload.Title != "🟢 encrypted turn — done" || payload.Body != plaintextMarker+" finished cleanly." {
		t.Fatalf("decrypted notification = %#v", payload)
	}
	select {
	case duplicate := <-received:
		t.Fatalf("structured completion double-fired: %#v", decryptPushPayload(t, duplicate.body, subscription))
	case <-time.After(120 * time.Millisecond):
	}
}

func TestWorkingToIdleWaitsForSustainedThreshold(t *testing.T) {
	mock, received := newCapturedPushServer(t)
	config, launcher, manager := newPushTestManager(t)
	manager.Push().SetHTTPClient(mock.Client())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: config.DefaultCwd, Name: "waiting edge",
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher.Runner(created.ID).AddOutput(strings.Repeat("completed work ", 50) + "\nThis command needs filesystem access. Continue? [y/N]\n")
	select {
	case early := <-received:
		t.Fatalf("waiting push fired before the sustained threshold: %#v", early)
	case <-time.After(35 * time.Millisecond):
	}
	select {
	case push := <-received:
		payload := decryptPushPayload(t, push.body, subscription)
		if payload.Title != "🟡 waiting edge — needs you" {
			t.Fatalf("waiting notification = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("working to idle notification was not observed after the sustained threshold")
	}
}

func TestSessionNotificationCooldownCoalescesRapidCompletions(t *testing.T) {
	mock, received := newCapturedPushServer(t)
	config, launcher, manager := newPushTestManager(t)
	manager.Push().SetHTTPClient(mock.Client())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: config.DefaultCwd, Name: "cooldown", Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := launcher.Runner(created.ID)
	emitCodexTurn(runner, "turn-1", "first completion")
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("first completion did not send")
	}
	emitCodexTurn(runner, "turn-2", "latest completion")
	select {
	case duplicate := <-received:
		t.Fatalf("rapid completion bypassed the cooldown: %#v", duplicate)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestNotifyTogglesSuppressAndPreservePerKind(t *testing.T) {
	mock, received := newCapturedPushServer(t)
	config, launcher, manager := newPushTestManager(t)
	manager.Push().SetHTTPClient(mock.Client())
	handler := New(config, manager, manager.Push())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	off := serve(t, handler, http.MethodPost, "/api/notify", bytes.NewBufferString(`{"enabled":false}`), "127.0.0.1:1", nil)
	if off.Code != http.StatusOK {
		t.Fatalf("notify off status=%d body=%s", off.Code, off.Body.String())
	}
	muted, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: config.DefaultCwd, Name: "muted", Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexTurn(launcher.Runner(muted.ID), "muted-turn", "must not send")
	select {
	case unexpected := <-received:
		t.Fatalf("notify off did not suppress completion: %#v", unexpected)
	case <-time.After(80 * time.Millisecond):
	}
	onDone := serve(t, handler, http.MethodPost, "/api/notify", bytes.NewBufferString(`{"enabled":true,"kind":"done"}`), "127.0.0.1:1", nil)
	if onDone.Code != http.StatusOK {
		t.Fatalf("notify done on status=%d body=%s", onDone.Code, onDone.Body.String())
	}
	var status notifyState
	decodeBody(t, onDone, &status)
	if !status.Notify.Done || status.Notify.Waiting || status.Notify.Lost || !status.Subscribed {
		t.Fatalf("per-kind notification state = %#v", status)
	}
	enabled, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: config.DefaultCwd, Name: "enabled", Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexTurn(launcher.Runner(enabled.ID), "enabled-turn", "done is enabled")
	select {
	case push := <-received:
		if payload := decryptPushPayload(t, push.body, subscription); payload.Title != "🟢 enabled — done" {
			t.Fatalf("enabled per-kind notification = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("per-kind done toggle did not enable completion push")
	}
}

func TestRunnerLostSendsEncryptedRecoveryPush(t *testing.T) {
	mock, received := newCapturedPushServer(t)
	config, launcher, manager := newPushTestManager(t)
	manager.Push().SetHTTPClient(mock.Client())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: config.DefaultCwd, Name: "lost work",
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher.Runner(created.ID).Emit(proto.Event{Kind: proto.EventRunnerLost, Exit: proto.ExitEvent{Reason: "runner-lost"}})
	select {
	case push := <-received:
		payload := decryptPushPayload(t, push.body, subscription)
		if payload.Title != "🔴 lost work — lost (sessions recover)" {
			t.Fatalf("lost notification = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner_lost did not send a recovery notification")
	}
}

func TestLaneDeathSendsEncryptedPushToMock(t *testing.T) {
	bodies := make(chan []byte, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read mock push body: %v", err)
		}
		bodies <- body
		response.WriteHeader(http.StatusCreated)
	}))
	defer mock.Close()
	config, launcher, manager := newPushTestManager(t)
	manager.Push().SetHTTPClient(mock.Client())
	subscription := validPushSubscription(t, mock.URL)
	if err := manager.Push().AddSubscription(subscription.value); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: config.DefaultCwd, Name: "scratch death", Kind: state.KindLane,
	})
	if err != nil {
		t.Fatal(err)
	}
	code := 7
	launcher.Runner(created.ID).Emit(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &code}})
	select {
	case body := <-bodies:
		payload := decryptPushPayload(t, body, subscription)
		if payload.Title != "🔴 scratch death died (exit 7)" {
			t.Fatalf("lane death notification = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mock endpoint did not receive the lane death notification")
	}
}

func newPushTestManager(t *testing.T) (state.Config, *prototest.Launcher, *sessionruntime.Manager) {
	t.Helper()
	return newPushTestManagerWithTiming(t, 50*time.Millisecond, 250*time.Millisecond)
}

func newPushTestManagerWithTiming(t *testing.T, waitingDelay, cooldown time.Duration) (state.Config, *prototest.Launcher, *sessionruntime.Manager) {
	t.Helper()
	root := t.TempDir()
	config := state.Config{
		Host: "127.0.0.1", Port: 0,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot:       filepath.Join(root, "state"),
		UserStateRoot:   filepath.Join(root, "user-state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: 10 * time.Millisecond,
		NotifyWaitingDelay: waitingDelay, NotifyCooldown: cooldown,
	})
	t.Cleanup(manager.Close)
	return config, launcher, manager
}

func newCapturedPushServer(t *testing.T) (*httptest.Server, <-chan capturedPush) {
	t.Helper()
	received := make(chan capturedPush, 8)
	mock := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read mock push body: %v", err)
		}
		received <- capturedPush{method: request.Method, header: request.Header.Clone(), body: body}
		response.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(mock.Close)
	return mock, received
}

func emitCodexTurn(runner *prototest.Runner, turnID, message string) {
	runner.AddCodexEvent(map[string]any{
		"type": "codex", "subtype": "turn_started", "source": "codex-app-server", "turnId": turnID,
	})
	runner.AddCodexEvent(map[string]any{
		"type": "assistant", "subtype": "item_completed", "source": "codex-app-server", "turnId": turnID,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": message}}},
	})
	runner.AddCodexEvent(map[string]any{
		"type": "codex", "subtype": "turn_completed", "source": "codex-app-server", "turnId": turnID,
	})
}

func validPushSubscription(t *testing.T, endpoint string) pushTestSubscription {
	t.Helper()
	privateKey, x, y, err := elliptic.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := elliptic.Marshal(elliptic.P256(), x, y)
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return pushTestSubscription{
		value: map[string]any{
			"endpoint": endpoint, "expirationTime": nil,
			"keys": map[string]string{
				"p256dh": base64.RawURLEncoding.EncodeToString(publicKey),
				"auth":   base64.RawURLEncoding.EncodeToString(auth),
			},
		},
		privateKey: privateKey, publicKey: publicKey, auth: auth,
	}
}

func decryptPushPayload(t *testing.T, body []byte, subscription pushTestSubscription) sessionruntime.PushPayload {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("encrypted push is too short: %d", len(body))
	}
	salt := body[:16]
	recordSize := binary.BigEndian.Uint32(body[16:20])
	serverKeyLength := int(body[20])
	headerLength := 21 + serverKeyLength
	if recordSize == 0 || serverKeyLength == 0 || len(body) <= headerLength {
		t.Fatalf("invalid encrypted push header: record=%d key=%d bytes=%d", recordSize, serverKeyLength, len(body))
	}
	serverPublicKey := body[21:headerLength]
	serverX, serverY := elliptic.Unmarshal(elliptic.P256(), serverPublicKey)
	if serverX == nil {
		t.Fatal("invalid server public key in encrypted push")
	}
	sharedX, _ := elliptic.P256().ScalarMult(serverX, serverY, subscription.privateKey)
	sharedSecret := make([]byte, 32)
	sharedX.FillBytes(sharedSecret)
	info := append([]byte("WebPush: info\x00"), subscription.publicKey...)
	info = append(info, serverPublicKey...)
	ikm := hkdfBytes(t, sharedSecret, subscription.auth, info, 32)
	contentKey := hkdfBytes(t, ikm, salt, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfBytes(t, ikm, salt, []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(contentKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := gcm.Open(nil, nonce, body[headerLength:], nil)
	if err != nil {
		t.Fatalf("decrypt push: %v", err)
	}
	delimiter := bytes.IndexByte(plaintext, 0x02)
	if delimiter < 0 {
		t.Fatal("decrypted push has no record delimiter")
	}
	for _, padding := range plaintext[delimiter+1:] {
		if padding != 0 {
			t.Fatal("decrypted push has invalid padding")
		}
	}
	var payload sessionruntime.PushPayload
	if err := json.Unmarshal(plaintext[:delimiter], &payload); err != nil {
		t.Fatalf("decode decrypted push %q: %v", plaintext[:delimiter], err)
	}
	return payload
}

func hkdfBytes(t *testing.T, secret, salt, info []byte, size int) []byte {
	t.Helper()
	value := make([]byte, size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, info), value); err != nil {
		t.Fatal(err)
	}
	return value
}
