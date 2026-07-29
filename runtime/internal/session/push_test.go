package session

import (
	"context"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPushStorageAndGoneSubscriptionCleanup(t *testing.T) {
	root := t.TempDir()
	service := NewPushService(root)
	publicKey, err := service.VAPIDPublicKey()
	if err != nil || publicKey == "" {
		t.Fatalf("VAPIDPublicKey() = %q, %v", publicKey, err)
	}
	assertMode(t, filepath.Join(root, "vapid.json"), 0o600)

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	_, x, y, err := elliptic.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic := base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
	authBytes := make([]byte, 16)
	if _, err := rand.Read(authBytes); err != nil {
		t.Fatal(err)
	}
	subscription := map[string]any{
		"endpoint": server.URL, "expirationTime": nil,
		"keys": map[string]string{"p256dh": clientPublic, "auth": base64.RawURLEncoding.EncodeToString(authBytes)},
	}
	if err := service.AddSubscription(subscription); err != nil {
		t.Fatal(err)
	}
	if err := service.AddSubscription(map[string]any{"endpoint": ""}); err == nil {
		t.Fatal("invalid subscription was accepted")
	}
	service.Send(context.Background(), PushPayload{Title: "done"})
	encoded, err := os.ReadFile(filepath.Join(root, "push-subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var remaining []any
	if err := json.Unmarshal(encoded, &remaining); err != nil || len(remaining) != 0 {
		t.Fatalf("stale subscriptions = %s, err=%v", encoded, err)
	}
	assertMode(t, filepath.Join(root, "push-subscriptions.json"), 0o600)
}
