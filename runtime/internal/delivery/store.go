// Package delivery persists the outcome of one logical composer submission.
//
// A request can outlive the HTTP connection that started it. Recording intent
// before writing to the runner makes a lost response survivable: a retry with
// the same operation id reads the existing receipt instead of sending the
// message twice.
package delivery

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type Status string

const (
	StatusPending      Status = "pending"
	StatusAccepted     Status = "accepted"
	StatusNotDelivered Status = "not-delivered"
	StatusUnknown      Status = "unknown"
	StatusTextOnly     Status = "text-delivered"
)

var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Record struct {
	OperationID  string `json:"operation_id"`
	SessionID    string `json:"session_id"`
	ContentHash  string `json:"content_sha256"`
	ContentBytes int    `json:"content_bytes"`
	Status       Status `json:"status"`
	Delivered    bool   `json:"delivered"`
	Retry        bool   `json:"retry"`
	Reason       string `json:"reason,omitempty"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	UpdatedAtMS  int64  `json:"updated_at_ms"`
}

type Store struct {
	root string
	mu   sync.Mutex
	now  func() time.Time
}

func New(root string) *Store {
	return &Store{root: filepath.Join(root, "delivery-operations"), now: time.Now}
}

func NewOperationID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func ValidateOperationID(value string) error {
	if !operationIDPattern.MatchString(value) {
		return errors.New("operation_id must be a lowercase UUID v4")
	}
	return nil
}

// Begin writes the indeterminate boundary before any runner input. If the
// same operation already exists, created is false and the stored result is
// returned. Reusing an id for different content or a different target is
// rejected rather than silently deduplicating the wrong message.
func (s *Store) Begin(operationID, sessionID, content string) (record Record, created bool, err error) {
	if err := ValidateOperationID(operationID); err != nil {
		return Record{}, false, err
	}
	if sessionID == "" {
		return Record{}, false, errors.New("session id is required")
	}
	hash := sha256.Sum256([]byte(content))
	now := s.now().UnixMilli()
	wanted := Record{
		OperationID: operationID, SessionID: sessionID,
		ContentHash: fmt.Sprintf("%x", hash[:]), ContentBytes: len([]byte(content)),
		Status: StatusPending, Retry: false, CreatedAtMS: now, UpdatedAtMS: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return Record{}, false, fmt.Errorf("create delivery operation directory: %w", err)
	}
	path := s.path(operationID)
	encoded, err := json.Marshal(wanted)
	if err != nil {
		return Record{}, false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(encoded); writeErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return Record{}, false, fmt.Errorf("write delivery operation: %w", writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			_ = os.Remove(path)
			return Record{}, false, fmt.Errorf("close delivery operation: %w", closeErr)
		}
		return wanted, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return Record{}, false, fmt.Errorf("create delivery operation: %w", err)
	}
	existing, err := read(path)
	if err != nil {
		return Record{}, false, err
	}
	if existing.SessionID != wanted.SessionID || existing.ContentHash != wanted.ContentHash || existing.ContentBytes != wanted.ContentBytes {
		return Record{}, false, errors.New("operation_id is already assigned to a different message")
	}
	return existing, false, nil
}

func (s *Store) Get(operationID string) (Record, error) {
	if err := ValidateOperationID(operationID); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return read(s.path(operationID))
}

func (s *Store) Complete(operationID string, status Status, delivered, retry bool, reason string) (Record, error) {
	if !validTerminalStatus(status) {
		return Record{}, fmt.Errorf("invalid delivery status %q", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(operationID)
	record, err := read(path)
	if err != nil {
		return Record{}, err
	}
	if record.Status != StatusPending {
		return record, nil
	}
	record.Status = status
	record.Delivered = delivered
	record.Retry = retry
	record.Reason = reason
	record.UpdatedAtMS = s.now().UnixMilli()
	if err := writeAtomic(path, record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) path(operationID string) string {
	return filepath.Join(s.root, operationID+".json")
}

func validTerminalStatus(status Status) bool {
	switch status {
	case StatusAccepted, StatusNotDelivered, StatusUnknown, StatusTextOnly:
		return true
	default:
		return false
	}
}

func read(path string) (Record, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(encoded, &record); err != nil {
		return Record{}, fmt.Errorf("read delivery operation: %w", err)
	}
	if err := ValidateOperationID(record.OperationID); err != nil {
		return Record{}, fmt.Errorf("read delivery operation: %w", err)
	}
	return record, nil
}

func writeAtomic(path string, record Record) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("write delivery operation: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("protect delivery operation: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("commit delivery operation: %w", err)
	}
	return nil
}
