package relayauth

import (
	"crypto/ed25519"
	"encoding/base64"
)

const Domain = "sessions-relay-v1"

type Challenge struct {
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
}

type Response struct {
	MachineID string `json:"machine_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

func Canonical(machineID string, challenge Challenge) []byte {
	return []byte(Domain + "\x00" + machineID + "\x00" + challenge.Timestamp + "\x00" + challenge.Nonce)
}

func Verify(response Response, challenge Challenge) bool {
	public, publicErr := base64.RawURLEncoding.DecodeString(response.PublicKey)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(response.Signature)
	return publicErr == nil && signatureErr == nil && len(public) == ed25519.PublicKeySize &&
		len(signature) == ed25519.SignatureSize &&
		ed25519.Verify(ed25519.PublicKey(public), Canonical(response.MachineID, challenge), signature)
}
