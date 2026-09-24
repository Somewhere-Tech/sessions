package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type AccountIdentity struct {
	// AccountID is the provider's own stable account or workspace identifier,
	// present only when the provider reports one. Two homes are the same
	// allowance only when they share it; an email alone cannot prove that.
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email"`
	Plan         string `json:"plan,omitempty"`
	Organization string `json:"organization,omitempty"`
	CheckedAt    int64  `json:"checked_at"`
}

type AccountLoginStatus struct {
	ID        string           `json:"id"`
	Tool      string           `json:"tool"`
	Profile   string           `json:"profile"`
	State     string           `json:"state"`
	URL       string           `json:"url,omitempty"`
	Code      string           `json:"code,omitempty"`
	Message   string           `json:"message,omitempty"`
	Identity  *AccountIdentity `json:"identity,omitempty"`
	ExpiresAt int64            `json:"expires_at"`
}

type accountLoginOperation struct {
	mu     sync.Mutex
	status AccountLoginStatus
	cancel context.CancelFunc
	input  io.WriteCloser
}

func (op *accountLoginOperation) snapshot() AccountLoginStatus {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.status
}

func (op *accountLoginOperation) update(fn func(*AccountLoginStatus)) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.status.State != "cancelled" {
		fn(&op.status)
	}
}

func newAccountLoginID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

// StartAccountLogin is idempotent while this profile has an active sign-in.
// It owns only a short-lived authentication helper, never an agent session.
func (m *Manager) StartAccountLogin(tool, name string) (AccountLoginStatus, error) {
	if err := state.ValidateProfileName(name); err != nil {
		return AccountLoginStatus{}, err
	}
	if tool != "claude" && tool != "codex" {
		return AccountLoginStatus{}, errors.New("choose Claude or ChatGPT")
	}
	home := m.AccountHomePath(tool, name)
	info, err := os.Lstat(home)
	if err != nil || !info.IsDir() || readAccountSidecar(m.config.UserStateRoot, tool, name).Removed {
		return AccountLoginStatus{}, errors.New("add this account on this computer before signing in")
	}
	m.accountLoginMu.Lock()
	defer m.accountLoginMu.Unlock()
	if m.accountLogins == nil {
		m.accountLogins = make(map[string]*accountLoginOperation)
	}
	for id, op := range m.accountLogins {
		s := op.snapshot()
		if s.ExpiresAt < time.Now().UnixMilli() {
			op.cancel()
			delete(m.accountLogins, id)
			continue
		}
		if s.Tool == tool && s.Profile == name && (s.State == "opening" || s.State == "waiting") {
			return s, nil
		}
	}
	if len(m.accountLogins) >= 32 {
		return AccountLoginStatus{}, errors.New("too many recent sign-ins; wait a few minutes and try again")
	}
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	op := &accountLoginOperation{cancel: cancel, status: AccountLoginStatus{
		ID: newAccountLoginID(), Tool: tool, Profile: name, State: "opening", ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(),
	}}
	m.accountLogins[op.status.ID] = op
	go m.runAccountLogin(ctx, op, home)
	return op.snapshot(), nil
}

func (m *Manager) AccountLogin(id, code string, cancel bool) (AccountLoginStatus, error) {
	m.accountLoginMu.Lock()
	op := m.accountLogins[id]
	m.accountLoginMu.Unlock()
	if op == nil {
		return AccountLoginStatus{}, errors.New("sign-in is no longer available; start again")
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if cancel {
		op.cancel()
		op.status.State, op.status.URL, op.status.Code = "cancelled", "", ""
	} else if code != "" {
		if op.status.Tool != "claude" || op.status.State != "waiting" || op.input == nil {
			return AccountLoginStatus{}, errors.New("this sign-in is not waiting for a code")
		}
		if len(code) > 2048 || strings.ContainsAny(code, "\r\n\x00") {
			return AccountLoginStatus{}, errors.New("paste just the sign-in code")
		}
		if _, err := io.WriteString(op.input, code+"\n"); err != nil {
			return AccountLoginStatus{}, errors.New("could not send code; start sign-in again")
		}
		op.input = nil
	}
	return op.status, nil
}

func (m *Manager) runAccountLogin(ctx context.Context, op *accountLoginOperation, home string) {
	defer op.cancel()
	s := op.snapshot()
	identity, err := loginProviderAccount(ctx, s.Tool, home, op)
	if err == nil {
		m.accountMetadataMu.Lock()
		sidecar := readAccountSidecar(m.config.UserStateRoot, s.Tool, s.Profile)
		sidecar.Identity = identity
		err = writeAccountSidecar(m.config.UserStateRoot, s.Tool, s.Profile, sidecar)
		m.accountMetadataMu.Unlock()
	}
	op.update(func(s *AccountLoginStatus) {
		s.URL, s.Code = "", ""
		if err == nil {
			s.State, s.Identity = "connected", identity
			return
		}
		s.State, s.Message = "failed", "Sign-in did not complete. Try again; existing accounts were not changed."
		if ctx.Err() != nil {
			s.State, s.Message = "expired", "Sign-in expired. Start again when you are ready."
		}
	})
}
