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
	// AccountID is reserved for a provider's own stable account or workspace
	// identifier. No provider Sessions supports reports one today: Codex
	// account/read and Claude auth status give an email and plan. Two homes are
	// the same allowance only when they share an AccountID; an email alone
	// cannot prove that, so homes on different computers stay unresolved until
	// a verified source exists. Never derive it from a credential or token.
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email"`
	Plan         string `json:"plan,omitempty"`
	Organization string `json:"organization,omitempty"`
	CheckedAt    int64  `json:"checked_at"`
}

// What the most recent provider check of an account's home established. Only a
// parsed provider answer can say a home is signed out or on another kind of
// login; a helper that failed, was cancelled or expired establishes nothing.
const (
	AccountCheckVerified        = "verified"
	AccountCheckSignedOut       = "signed_out"
	AccountCheckNotSubscription = "not_subscription"
	AccountCheckFailed          = "failed"
	// AccountCheckNotChecked marks a forgotten account added again: whatever an
	// earlier check found is history until the home is checked anew.
	AccountCheckNotChecked = "not_checked"
)

type AccountCheck struct {
	At      int64  `json:"at"`
	Outcome string `json:"outcome"`
}

var (
	errAccountNotSubscription  = errors.New("the provider reports a sign-in here that is not a subscription account")
	errAccountStatusUnreadable = errors.New("the provider's account status could not be read")
)

// accountCheckNotSavedError is a provider answer Sessions could not persist.
// The answer was real, but the account list cannot show it, so the helper says
// so instead of acknowledging what it did not keep. beforeLogin marks an
// answer saved before any provider sign-in started, where the helper stops;
// a final answer may follow a sign-in the provider already completed, so its
// message claims nothing about the provider's sign-in being unchanged.
type accountCheckNotSavedError struct {
	outcome     string
	beforeLogin bool
	err         error
}

func (e *accountCheckNotSavedError) Error() string { return "save account check: " + e.err.Error() }
func (e *accountCheckNotSavedError) Unwrap() error { return e.err }

func (e *accountCheckNotSavedError) message() string {
	found := map[string]string{
		AccountCheckVerified:        "The provider confirmed the sign-in",
		AccountCheckSignedOut:       "The provider reported no sign-in here",
		AccountCheckNotSubscription: "The provider reports a sign-in here that is not a subscription",
	}[e.outcome]
	if found == "" {
		found = "Sessions could not read who is signed in"
	}
	next := "Free disk space, then check the account again. Sessions does not repeat a sign-in or sign out on its own."
	if e.beforeLogin {
		next = "Sign-in was not started. Free disk space, then check the account again."
	}
	return found + ", but Sessions could not save this check (" + e.err.Error() + "), so the account list may still show the earlier one. " + next
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
	// record persists a provider observation of this home; observed says one
	// was recorded already. Both belong to the helper's own goroutine.
	record   func(AccountCheck, *AccountIdentity) error
	observed bool
	// done closes once the helper has finished and recorded what it found.
	done chan struct{}
}

// observe records what a provider status read established mid-check, so a
// later cancellation or expiry of the sign-in cannot erase it. An answer that
// could not be saved is not acknowledged: the caller stops with its error
// rather than starting a sign-in the account list cannot account for.
func (op *accountLoginOperation) observe(outcome string) error {
	if op.record != nil {
		if err := op.record(AccountCheck{At: time.Now().UnixMilli(), Outcome: outcome}, nil); err != nil {
			return &accountCheckNotSavedError{outcome: outcome, beforeLogin: true, err: err}
		}
	}
	op.observed = true
	return nil
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
	op := &accountLoginOperation{cancel: cancel, done: make(chan struct{}), status: AccountLoginStatus{
		ID: newAccountLoginID(), Tool: tool, Profile: name, State: "opening", ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(),
	}}
	op.record = func(check AccountCheck, identity *AccountIdentity) error {
		// A new answer about who is signed in outdates every earlier reading,
		// including while the sign-in it led to is still open.
		m.accountUsage.invalidate(tool, name)
		return m.recordAccountCheck(tool, name, check, identity)
	}
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
	defer close(op.done)
	defer op.cancel()
	s := op.snapshot()
	identity, err := loginProviderAccount(ctx, s.Tool, home, op)
	// Whatever the outcome, the provider may have changed who is signed in
	// here; no earlier usage reading of this home can be trusted to match.
	m.accountUsage.invalidate(s.Tool, s.Profile)
	if check, ok := finalAccountCheck(ctx, op.observed, identity, err); ok && op.record != nil {
		if recordErr := op.record(check, identity); recordErr != nil {
			err = &accountCheckNotSavedError{outcome: check.Outcome, err: recordErr}
		}
	}
	state, message := accountLoginOutcome(ctx, err)
	if state == "failed" && errors.Is(err, errAccountProviderUnavailable) {
		message = accountLoginFailureMessage(s.Tool, err)
	}
	op.update(func(s *AccountLoginStatus) {
		s.URL, s.Code = "", ""
		s.State, s.Message = state, message
		if err == nil {
			s.Identity = identity
		}
	})
}

// Only our own error categories reach the UI; provider errors may contain
// authentication details. Explain missing executables without publishing them.
func accountLoginFailureMessage(tool string, err error) string {
	if !errors.Is(err, errAccountProviderUnavailable) {
		return "Sign-in did not complete. Try again; your other account sign-ins were not changed."
	}
	provider := "Claude Code"
	if tool == "codex" {
		provider = "Codex"
	}
	return "Sessions could not find " + provider + " on this computer. Install it, then try Sign in again."
}

// accountLoginOutcome is the state and instruction a finished helper reports.
// A check Sessions could not save says so first: reporting it as connected or
// failed alone would hide that the account list is behind the provider.
func accountLoginOutcome(ctx context.Context, err error) (string, string) {
	var notSaved *accountCheckNotSavedError
	switch {
	case err == nil:
		return "connected", ""
	case errors.As(err, &notSaved):
		return "failed", notSaved.message()
	case ctx.Err() != nil:
		return "expired", "Sign-in expired. Start again when you are ready."
	case errors.Is(err, errAccountNotSubscription):
		return "failed", "The provider reports a sign-in here that is not a subscription, such as an API key. Sessions did not change it; sign in with a subscription account to use this account."
	}
	return "failed", accountLoginFailureMessage("", err)
}

// finalAccountCheck decides what a finished helper established. A success is
// a verified identity. A provider answer that the login is not a subscription,
// or a status read that failed, is recorded as such. A helper that could not
// start counts as a failed check unless a provider answer was recorded earlier
// in this helper. A cancelled or expired helper records nothing: the person
// stopping it says nothing about the home. An answer that could not be saved
// is reported, not replaced by a weaker outcome written over it.
func finalAccountCheck(ctx context.Context, observed bool, identity *AccountIdentity, err error) (AccountCheck, bool) {
	var notSaved *accountCheckNotSavedError
	switch {
	case errors.As(err, &notSaved):
		return AccountCheck{}, false
	case err == nil && identity != nil:
		return AccountCheck{At: identity.CheckedAt, Outcome: AccountCheckVerified}, true
	case errors.Is(err, errAccountNotSubscription):
		return AccountCheck{At: time.Now().UnixMilli(), Outcome: AccountCheckNotSubscription}, true
	case err == nil || ctx.Err() != nil:
		return AccountCheck{}, false
	case errors.Is(err, errAccountStatusUnreadable) || !observed:
		return AccountCheck{At: time.Now().UnixMilli(), Outcome: AccountCheckFailed}, true
	}
	return AccountCheck{}, false
}

// recordAccountCheck persists one provider observation of an account's home
// unless a newer observation is already recorded there.
func (m *Manager) recordAccountCheck(tool, name string, check AccountCheck, identity *AccountIdentity) error {
	m.accountMetadataMu.Lock()
	defer m.accountMetadataMu.Unlock()
	sidecar := readAccountSidecar(m.config.UserStateRoot, tool, name)
	if !sidecar.observe(check, identity) {
		return nil
	}
	return writeAccountSidecar(m.config.UserStateRoot, tool, name, sidecar)
}
