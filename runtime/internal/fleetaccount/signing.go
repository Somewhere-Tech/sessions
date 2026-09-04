package fleetaccount

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	machineIDHeader = "X-Sessions-Machine-ID"
	timestampHeader = "X-Sessions-Timestamp"
	nonceHeader     = "X-Sessions-Nonce"
	signatureHeader = "X-Sessions-Signature"
)

func canonicalRequest(machineID, timestamp, nonce, method, path string, body []byte) []byte {
	hash := sha256.Sum256(body)
	return []byte(machineID + timestamp + nonce + method + path + hex.EncodeToString(hash[:]))
}

func signRequest(request *http.Request, body []byte, machineID string, key ed25519.PrivateKey, now time.Time) error {
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate signed-request nonce: %w", err)
	}
	timestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	canonical := canonicalRequest(machineID, timestamp, nonce, request.Method, request.URL.EscapedPath(), body)
	signature := ed25519.Sign(key, canonical)
	request.Header.Set(machineIDHeader, machineID)
	request.Header.Set(timestampHeader, timestamp)
	request.Header.Set(nonceHeader, nonce)
	request.Header.Set(signatureHeader, base64.RawURLEncoding.EncodeToString(signature))
	return nil
}
