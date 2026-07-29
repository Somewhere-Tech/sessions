package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	pairTicketTTL            = 5 * time.Minute
	pairFailureWindow        = time.Minute
	pairFailureLimit         = 10
	deviceLastUsedWriteGap   = time.Minute
	maximumPairingDeviceName = 80
)

var (
	errPairTicketGone = errors.New("pairing ticket is invalid, expired, or already used")
	errPairRateLimit  = errors.New("too many failed pairing attempts")
)

const pairTicketGoneMessage = "Pairing ticket is invalid, expired, or already used. Run `sessions pair` to create a new one."

type pairTicket struct {
	ID        string
	Secret    string
	Name      string
	ExpiresAt time.Time
}

type pairService struct {
	mu           sync.Mutex
	tickets      map[string]pairTicket
	failedClaims []time.Time
	now          func() time.Time
	devices      *deviceStore
}

type pairingTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pairingClaimResponse struct {
	DeviceID    string `json:"device_id"`
	Token       string `json:"token"`
	Name        string `json:"name"`
	MachineID   string `json:"machine_id,omitempty"`
	MachineName string `json:"machine_name,omitempty"`
}

type deviceRecord struct {
	DeviceID     string     `json:"device_id"`
	Name         string     `json:"name"`
	TokenHash    string     `json:"token_hash"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   time.Time  `json:"last_used_at"`
	PendingUntil *time.Time `json:"pending_until,omitempty"`
}

type deviceView struct {
	DeviceID   string    `json:"device_id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type deviceFile struct {
	Devices []deviceRecord `json:"devices"`
}

type deviceStore struct {
	path          string
	mu            sync.Mutex
	loaded        bool
	records       map[string]deviceRecord
	lastPersisted map[string]time.Time
	now           func() time.Time
}

func newPairService(config state.Config) *pairService {
	root := config.UserStateRoot
	if root == "" {
		root = config.StateRoot
	}
	now := time.Now
	return &pairService{
		tickets: make(map[string]pairTicket),
		now:     now,
		devices: newDeviceStore(filepath.Join(root, "devices.json"), now),
	}
}

func newDeviceStore(path string, now func() time.Time) *deviceStore {
	return &deviceStore{
		path: path, records: make(map[string]deviceRecord),
		lastPersisted: make(map[string]time.Time), now: now,
	}
}

func (p *pairService) setNow(now func() time.Time) {
	p.mu.Lock()
	p.now = now
	p.mu.Unlock()
	p.devices.mu.Lock()
	p.devices.now = now
	p.devices.mu.Unlock()
}

func (p *pairService) mint(name string) (pairingTicketResponse, error) {
	id, err := randomBase64URL(16)
	if err != nil {
		return pairingTicketResponse{}, fmt.Errorf("generate pairing ticket id: %w", err)
	}
	secret, err := randomBase64URL(32)
	if err != nil {
		return pairingTicketResponse{}, fmt.Errorf("generate pairing ticket secret: %w", err)
	}
	name = truncateDeviceName(name)

	p.mu.Lock()
	defer p.mu.Unlock()
	expiresAt := p.now().UTC().Add(pairTicketTTL)
	p.tickets[id] = pairTicket{ID: id, Secret: secret, Name: name, ExpiresAt: expiresAt}
	return pairingTicketResponse{Ticket: id + "." + secret, ExpiresAt: expiresAt}, nil
}

func (p *pairService) claim(encoded, name, userAgent string) (pairingClaimResponse, error) {
	now := p.now().UTC()
	id, secret, validShape := strings.Cut(strings.TrimSpace(encoded), ".")
	if !validShape || id == "" || secret == "" || strings.Contains(secret, ".") {
		if p.recordFailedClaim(now) {
			return pairingClaimResponse{}, errPairRateLimit
		}
		return pairingClaimResponse{}, errPairTicketGone
	}

	p.mu.Lock()
	p.pruneFailuresLocked(now)
	if len(p.failedClaims) >= pairFailureLimit {
		p.mu.Unlock()
		return pairingClaimResponse{}, errPairRateLimit
	}
	ticket, found := p.tickets[id]
	validSecret := found && constantTimeStringEqual(secret, ticket.Secret)
	if !found || !validSecret || !now.Before(ticket.ExpiresAt) {
		if found && !now.Before(ticket.ExpiresAt) {
			delete(p.tickets, id)
		}
		p.failedClaims = append(p.failedClaims, now)
		p.mu.Unlock()
		return pairingClaimResponse{}, errPairTicketGone
	}
	deviceName := truncateDeviceName(name)
	if deviceName == "" {
		deviceName = ticket.Name
	}
	if deviceName == "" {
		deviceName = truncateDeviceName(userAgent)
	}
	if deviceName == "" {
		deviceName = "Unknown device"
	}
	record, token, err := p.devices.create(deviceName)
	if err != nil {
		p.mu.Unlock()
		return pairingClaimResponse{}, err
	}
	// Keep the ticket reserved while the credential is made durable. This
	// prevents concurrent claims and, if persistence fails, leaves the
	// single-use link available for a retry instead of burning it.
	delete(p.tickets, id)
	p.mu.Unlock()
	return pairingClaimResponse{DeviceID: record.DeviceID, Token: token, Name: record.Name}, nil
}

func (p *pairService) recordFailedClaim(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneFailuresLocked(now)
	if len(p.failedClaims) >= pairFailureLimit {
		return true
	}
	p.failedClaims = append(p.failedClaims, now)
	return false
}

func (p *pairService) pruneFailuresLocked(now time.Time) {
	cutoff := now.Add(-pairFailureWindow)
	first := 0
	for first < len(p.failedClaims) && !p.failedClaims[first].After(cutoff) {
		first++
	}
	if first > 0 {
		p.failedClaims = append([]time.Time(nil), p.failedClaims[first:]...)
	}
}

func (s *deviceStore) create(name string) (deviceRecord, string, error) {
	return s.createPending(name, time.Time{})
}

// createPending persists a credential which cannot authorize after pendingUntil
// unless the client proves receipt by using it once before then. The first
// successful authorization clears PendingUntil durably. This makes an accepted
// tailnet claim idempotent without leaving a live orphan if its HTTP response is
// lost.
func (s *deviceStore) createPending(name string, pendingUntil time.Time) (deviceRecord, string, error) {
	token, err := randomBase64URL(32)
	if err != nil {
		return deviceRecord{}, "", fmt.Errorf("generate device token: %w", err)
	}
	id, err := randomDeviceUUID()
	if err != nil {
		return deviceRecord{}, "", fmt.Errorf("generate device id: %w", err)
	}
	now := s.now().UTC()
	hash := sha256.Sum256([]byte(token))
	record := deviceRecord{
		DeviceID: id, Name: name, TokenHash: hex.EncodeToString(hash[:]),
		CreatedAt: now, LastUsedAt: now,
	}
	if !pendingUntil.IsZero() {
		expires := pendingUntil.UTC()
		record.PendingUntil = &expires
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return deviceRecord{}, "", err
	}
	if _, exists := s.records[id]; exists {
		return deviceRecord{}, "", errors.New("generated duplicate device id")
	}
	s.records[id] = record
	s.lastPersisted[id] = now
	if err := s.saveLocked(); err != nil {
		delete(s.records, id)
		delete(s.lastPersisted, id)
		return deviceRecord{}, "", err
	}
	return record, token, nil
}

func (s *deviceStore) authorize(token string) (bool, error) {
	_, authorized, err := s.authenticate(token)
	return authorized, err
}

func (s *deviceStore) authenticate(token string) (deviceRecord, bool, error) {
	if token == "" {
		return deviceRecord{}, false, nil
	}
	providedHash := sha256.Sum256([]byte(token))

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return deviceRecord{}, false, err
	}
	for id, record := range s.records {
		expectedHash, err := hex.DecodeString(record.TokenHash)
		if err != nil || len(expectedHash) != sha256.Size {
			return deviceRecord{}, false, fmt.Errorf("decode device token hash for %s", id)
		}
		if subtle.ConstantTimeCompare(providedHash[:], expectedHash) != 1 {
			continue
		}
		now := s.now().UTC()
		if record.PendingUntil != nil {
			if !now.Before(*record.PendingUntil) {
				lastPersisted := s.lastPersisted[id]
				delete(s.records, id)
				delete(s.lastPersisted, id)
				if err := s.saveLocked(); err != nil {
					s.records[id] = record
					s.lastPersisted[id] = lastPersisted
					return deviceRecord{}, false, err
				}
				return deviceRecord{}, false, nil
			}
			// The first authenticated API request is the acknowledgement that
			// the claimant received and durably stored this token.
			pendingRecord := record
			record.PendingUntil = nil
			record.LastUsedAt = now
			s.records[id] = record
			if err := s.saveLocked(); err != nil {
				s.records[id] = pendingRecord
				return deviceRecord{}, false, err
			}
			s.lastPersisted[id] = now
			return record, true, nil
		}
		record.LastUsedAt = now
		s.records[id] = record
		if now.Sub(s.lastPersisted[id]) >= deviceLastUsedWriteGap {
			if err := s.saveLocked(); err != nil {
				return deviceRecord{}, false, err
			}
			s.lastPersisted[id] = now
		}
		return record, true, nil
	}
	return deviceRecord{}, false, nil
}

func (s *deviceStore) list() ([]deviceView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	devices := make([]deviceView, 0, len(s.records))
	for _, record := range s.records {
		if record.PendingUntil != nil {
			continue
		}
		devices = append(devices, deviceView{
			DeviceID: record.DeviceID, Name: record.Name,
			CreatedAt: record.CreatedAt, LastUsedAt: record.LastUsedAt,
		})
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].CreatedAt.Equal(devices[j].CreatedAt) {
			return devices[i].DeviceID < devices[j].DeviceID
		}
		return devices[i].CreatedAt.Before(devices[j].CreatedAt)
	})
	return devices, nil
}

func (s *deviceStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	record, exists := s.records[id]
	if !exists {
		return false, nil
	}
	lastPersisted := s.lastPersisted[id]
	delete(s.records, id)
	delete(s.lastPersisted, id)
	if err := s.saveLocked(); err != nil {
		s.records[id] = record
		s.lastPersisted[id] = lastPersisted
		return false, err
	}
	return true, nil
}

func (s *deviceStore) loadLocked() error {
	if s.loaded {
		return nil
	}
	encoded, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read devices: %w", err)
	}
	var stored deviceFile
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return fmt.Errorf("decode devices: %w", err)
	}
	records := make(map[string]deviceRecord, len(stored.Devices))
	lastPersisted := make(map[string]time.Time, len(stored.Devices))
	prunedExpiredPending := false
	now := s.now().UTC()
	for _, record := range stored.Devices {
		if record.DeviceID == "" || record.Name == "" || record.CreatedAt.IsZero() || record.LastUsedAt.IsZero() {
			return errors.New("decode devices: invalid device record")
		}
		hash, err := hex.DecodeString(record.TokenHash)
		if err != nil || len(hash) != sha256.Size {
			return fmt.Errorf("decode devices: invalid token hash for %s", record.DeviceID)
		}
		if _, duplicate := records[record.DeviceID]; duplicate {
			return fmt.Errorf("decode devices: duplicate device id %s", record.DeviceID)
		}
		if record.PendingUntil != nil && !now.Before(*record.PendingUntil) {
			prunedExpiredPending = true
			continue
		}
		records[record.DeviceID] = record
		lastPersisted[record.DeviceID] = record.LastUsedAt
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("chmod devices: %w", err)
	}
	s.records = records
	s.lastPersisted = lastPersisted
	s.loaded = true
	if prunedExpiredPending {
		if err := s.saveLocked(); err != nil {
			s.loaded = false
			return fmt.Errorf("purge expired pending devices: %w", err)
		}
	}
	return nil
}

func (s *deviceStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create devices directory: %w", err)
	}
	records := make([]deviceRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].DeviceID < records[j].DeviceID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	encoded, err := json.Marshal(deviceFile{Devices: records})
	if err != nil {
		return fmt.Errorf("encode devices: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".devices-*")
	if err != nil {
		return fmt.Errorf("create temporary devices: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod temporary devices: %w", err)
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write devices: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close devices: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace devices: %w", err)
	}
	return nil
}

func (s *Server) handlePairClaimRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/pair/claim" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if s.identityError != nil || s.identity.ID == "" {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": "pairing is temporarily unavailable on this machine",
		}, corsOrigin)
		return true
	}
	var body struct {
		Ticket string `json:"ticket"`
		Name   string `json:"name"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	claimed, err := s.pair.claim(body.Ticket, body.Name, request.UserAgent())
	if errors.Is(err, errPairRateLimit) {
		s.sendJSON(response, http.StatusTooManyRequests, map[string]any{
			"error": "Too many failed pairing attempts. Wait one minute, then run `sessions pair` again.",
		}, corsOrigin)
		return true
	}
	if errors.Is(err, errPairTicketGone) {
		s.sendJSON(response, http.StatusGone, map[string]any{"error": pairTicketGoneMessage}, corsOrigin)
		return true
	}
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	claimed.MachineID = s.identity.ID
	claimed.MachineName = s.identity.Name
	s.sendJSON(response, http.StatusCreated, claimed, corsOrigin)
	return true
}

func (s *Server) handlePairRoutes(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	switch {
	case request.URL.Path == "/api/pair/ticket":
		if request.Method != http.MethodPost {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		ticket, err := s.pair.mint(body.Name)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusCreated, ticket, corsOrigin)
		return true
	case request.URL.Path == "/api/devices":
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		devices, err := s.pair.devices.list()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"devices": devices}, corsOrigin)
		return true
	case strings.HasPrefix(request.URL.Path, "/api/devices/"):
		if request.Method != http.MethodDelete {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		id := strings.TrimPrefix(request.URL.Path, "/api/devices/")
		if id == "" || strings.Contains(id, "/") {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "device not found"}, corsOrigin)
			return true
		}
		revoked, err := s.pair.devices.revoke(id)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if !revoked {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "device not found"}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true, "device_id": id}, corsOrigin)
		return true
	default:
		return false
	}
}

func randomBase64URL(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomDeviceUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func constantTimeStringEqual(provided, expected string) bool {
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func truncateDeviceName(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= maximumPairingDeviceName {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximumPairingDeviceName])
}
