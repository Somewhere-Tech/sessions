package fleetaccount

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"

	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

func (m *Manager) SignRelayChallenge(challenge relayauth.Challenge) (relayauth.Response, error) {
	key, public, err := m.keys.loadOrCreate()
	if err != nil {
		return relayauth.Response{}, err
	}
	response := relayauth.Response{MachineID: m.machineID, PublicKey: public}
	signature := ed25519.Sign(key, relayauth.Canonical(m.machineID, challenge))
	response.Signature = base64.RawURLEncoding.EncodeToString(signature)
	return response, nil
}

func (m *Manager) DirectoryRelay(ctx context.Context) string {
	state, err := m.state.load()
	if err != nil || !state.signedIn() {
		return ""
	}
	machines, err := m.Machines(ctx)
	if err != nil {
		return ""
	}
	for _, machine := range machines {
		if machine.ID == m.machineID {
			return machine.EndpointsJSON.Relay
		}
	}
	return ""
}
