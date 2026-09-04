package fleetaccount

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	AccountClaimPath = "/api/lan/access/account-claim"
	claimWindow      = 5 * time.Minute
)

var (
	ErrClaimInvalid   = errors.New("account claim is invalid")
	ErrClaimExpired   = errors.New("account claim is outside the five-minute window")
	ErrClaimReplay    = errors.New("account claim nonce was already used")
	ErrDifferentOwner = errors.New("requesting device is not in this account")
)

type unsignedClaim struct {
	MachineID string `json:"machine_id"`
	DeviceID  string `json:"device_id"`
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
}

func (m *Manager) Machines(ctx context.Context) ([]Machine, error) {
	key, _, err := m.keys.loadOrCreate()
	if err != nil {
		return nil, err
	}
	body, err := m.cloud.request(ctx, http.MethodGet, "/api/machines/index", nil, true, m.machineID, key)
	if err != nil {
		return nil, err
	}
	var response struct {
		Machines []Machine `json:"machines"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("Somewhere fleet directory returned invalid machines")
	}
	if response.Machines == nil {
		response.Machines = []Machine{}
	}
	return response.Machines, nil
}

func (m *Manager) CreateAccountClaim(machineID string) (AccountClaim, error) {
	if strings.TrimSpace(machineID) == "" {
		return AccountClaim{}, errors.New("account claim requires a target machine id")
	}
	key, _, err := m.keys.loadOrCreate()
	if err != nil {
		return AccountClaim{}, err
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return AccountClaim{}, fmt.Errorf("generate account-claim nonce: %w", err)
	}
	claim := AccountClaim{
		MachineID: machineID, DeviceID: m.machineID,
		Timestamp: strconv.FormatInt(m.now().UTC().Unix(), 10),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
	}
	claim.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, accountClaimCanonical(claim)))
	return claim, nil
}

func (m *Manager) VerifyAccountClaim(ctx context.Context, claim AccountClaim) (Machine, error) {
	if claim.MachineID != m.machineID {
		return Machine{}, fmt.Errorf("%w: names a different machine", ErrClaimInvalid)
	}
	if err := m.validateClaimTime(claim.Timestamp); err != nil {
		return Machine{}, err
	}
	if claim.DeviceID == "" || claim.Nonce == "" || len(claim.Nonce) > 128 {
		return Machine{}, fmt.Errorf("%w: identity or nonce is invalid", ErrClaimInvalid)
	}
	machines, err := m.Machines(ctx)
	if err != nil {
		return Machine{}, err
	}
	device, found := findDirectoryMachine(machines, claim.DeviceID)
	if !found {
		return Machine{}, ErrDifferentOwner
	}
	public, err := base64.RawURLEncoding.DecodeString(device.MachinePublicKey)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(claim.Signature)
	if err != nil || len(public) != ed25519.PublicKeySize || signatureErr != nil || len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(ed25519.PublicKey(public), accountClaimCanonical(claim), signature) {
		return Machine{}, fmt.Errorf("%w: signature is invalid", ErrClaimInvalid)
	}
	if err := m.consumeClaimNonce(claim.DeviceID, claim.Nonce); err != nil {
		return Machine{}, err
	}
	return device, nil
}

func (m *Manager) validateClaimTime(value string) error {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || len(value) < 10 || len(value) > 12 {
		return fmt.Errorf("%w: timestamp is invalid", ErrClaimInvalid)
	}
	when := time.Unix(seconds, 0)
	if delta := m.now().UTC().Sub(when); delta < -claimWindow || delta > claimWindow {
		return ErrClaimExpired
	}
	return nil
}

func (m *Manager) consumeClaimNonce(deviceID, nonce string) error {
	m.claimsMu.Lock()
	defer m.claimsMu.Unlock()
	now := m.now().UTC()
	for key, acceptedAt := range m.claims {
		if now.Sub(acceptedAt) > claimWindow {
			delete(m.claims, key)
		}
	}
	key := deviceID + "\x00" + nonce
	if _, exists := m.claims[key]; exists {
		return ErrClaimReplay
	}
	m.claims[key] = now
	return nil
}

func findDirectoryMachine(machines []Machine, id string) (Machine, bool) {
	for _, machine := range machines {
		if machine.ID == id {
			return machine, true
		}
	}
	return Machine{}, false
}

func accountClaimCanonical(claim AccountClaim) []byte {
	unsigned := unsignedClaim{
		MachineID: claim.MachineID, DeviceID: claim.DeviceID,
		Timestamp: claim.Timestamp, Nonce: claim.Nonce,
	}
	body, _ := json.Marshal(unsigned)
	hash := sha256.Sum256(body)
	return []byte(claim.MachineID + claim.DeviceID + claim.Timestamp + claim.Nonce +
		http.MethodPost + AccountClaimPath + hex.EncodeToString(hash[:]))
}
