package fleetaccount

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestSignedRequestCoversIdentityTimeRouteAndExactBody(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := &http.Request{
		Method: http.MethodPost,
		URL:    &url.URL{Path: "/api/machines/register"},
		Header: make(http.Header),
	}
	body := []byte(`{"machine_id":"machine-one","name":"Mini"}`)
	now := time.Date(2026, 9, 3, 20, 10, 0, 0, time.UTC)
	if err := signRequest(request, body, "machine-one", private, now); err != nil {
		t.Fatal(err)
	}
	timestamp := request.Header.Get(timestampHeader)
	if timestamp != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("timestamp = %q", timestamp)
	}
	nonce := request.Header.Get(nonceHeader)
	signature, err := base64.RawURLEncoding.DecodeString(request.Header.Get(signatureHeader))
	if err != nil {
		t.Fatal(err)
	}
	canonical := canonicalRequest("machine-one", timestamp, nonce, http.MethodPost, request.URL.Path, body)
	if !ed25519.Verify(public, canonical, signature) {
		t.Fatal("signature does not verify over the canonical request")
	}
	if ed25519.Verify(public, canonicalRequest("machine-one", timestamp, nonce, http.MethodPost, request.URL.Path, append(body, ' ')), signature) {
		t.Fatal("signature verified after body changed")
	}
}
