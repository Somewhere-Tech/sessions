package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A profile is a second subscription: its own provider home, its own login, its
// own history. Sessions does not read credentials: directory markers remain a
// presence-only observation, while an explicit account check asks the provider
// to report its signed-in identity and records when that check happened.
//
// The label lives beside the profile directory rather than inside it, because
// the directory is the provider's own home and nothing of ours belongs in it.
// Removing an account writes `removed` there and leaves the home alone; a
// person's second subscription is not something to delete on a button press.

type accountSidecar struct {
	// Identity is present only while the most recent check verified it. A
	// newer check that did not moves it to PreviousIdentity as history, so
	// an older client reading only `identity` never shows it as current.
	Identity         *AccountIdentity `json:"identity,omitempty"`
	LastCheck        *AccountCheck    `json:"last_check,omitempty"`
	PreviousIdentity *AccountIdentity `json:"previous_identity,omitempty"`
	Label            string           `json:"label,omitempty"`
	Removed          bool             `json:"removed,omitempty"`
}

// lastCheckAt is when the newest recorded observation was made. A sidecar
// written before `last_check` existed dates from its identity check.
func (s accountSidecar) lastCheckAt() int64 {
	at := int64(0)
	if s.LastCheck != nil {
		at = s.LastCheck.At
	}
	if s.Identity != nil && s.Identity.CheckedAt > at {
		at = s.Identity.CheckedAt
	}
	return at
}

// observe applies one observation unless a newer one is already recorded, so
// a slow helper finishing late cannot overwrite a fresher answer. A re-add
// always applies, whatever the clock says. It reports whether the sidecar
// changed.
func (s *accountSidecar) observe(check AccountCheck, identity *AccountIdentity) bool {
	stale := check.Outcome != AccountCheckNotChecked && check.At < s.lastCheckAt()
	if stale || (check.Outcome == AccountCheckVerified && identity == nil) {
		return false
	}
	if check.Outcome == AccountCheckVerified {
		s.Identity, s.PreviousIdentity = identity, nil
	} else if s.Identity != nil {
		s.Identity, s.PreviousIdentity = nil, s.Identity
	}
	s.LastCheck = &check
	return true
}

// applyTo copies what this computer's checks established onto a listing.
func (s accountSidecar) applyTo(status *ProfileStatus) {
	status.Identity, status.LastCheck, status.PreviousIdentity = s.Identity, s.LastCheck, s.PreviousIdentity
}

// signedInMarkers are the files each provider writes when a login completes.
// Their presence is the whole reading: Sessions never opens them, and a
// provider that keeps its credential in the system keychain instead simply
// reads as not signed in here, which is why this is reported as a fact about
// the directory rather than as an account state.
var signedInMarkers = map[string][]string{
	"claude": {".credentials.json", "credentials.json"},
	"codex":  {"auth.json"},
}

// MaxAccountLabelLength is the longest nickname, in characters, an account
// keeps. The add form has always stopped at this length; the daemon now says so
// too, so a label from the CLI and one from the app follow the same rule.
const MaxAccountLabelLength = 64

// ValidateAccountLabel accepts any printable nickname up to the length limit.
// An empty label is allowed and means the account has no nickname.
func ValidateAccountLabel(label string) error {
	if !utf8.ValidString(label) {
		return errors.New("an account nickname must be readable text")
	}
	if utf8.RuneCountInString(label) > MaxAccountLabelLength {
		return fmt.Errorf("an account nickname can be at most %d characters", MaxAccountLabelLength)
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return errors.New("an account nickname cannot contain line breaks, tabs, or control characters")
		}
	}
	return nil
}

func accountSidecarPath(root, tool, name string) string {
	return filepath.Join(root, "profiles", tool, name+".account.json")
}

func readAccountSidecar(root, tool, name string) accountSidecar {
	raw, err := os.ReadFile(accountSidecarPath(root, tool, name))
	if err != nil {
		return accountSidecar{}
	}
	var sidecar accountSidecar
	if json.Unmarshal(raw, &sidecar) != nil {
		return accountSidecar{}
	}
	return sidecar
}

func writeAccountSidecar(root, tool, name string, sidecar accountSidecar) error {
	path := accountSidecarPath(root, tool, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	encoded, err := json.Marshal(sidecar)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		// A full disk can leave a partial file; the saved sidecar is untouched.
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// profileSignedIn reports whether this provider home carries the login state the
// provider writes. Presence only: no file here is opened or parsed.
func profileSignedIn(path, tool string) bool {
	for _, marker := range signedInMarkers[tool] {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true
		}
	}
	return false
}

// CreateAccount registers a named provider home so a person can log a second
// subscription into it. It creates the directory and records the label; the
// account-login helper then signs in with the provider inside that home.
func (m *Manager) CreateAccount(tool, name, label string) (ProfileStatus, error) {
	m.accountMetadataMu.Lock()
	defer m.accountMetadataMu.Unlock()
	if name == "" {
		name = "acct-" + newAccountLoginID()
	}
	if err := state.ValidateProfileName(name); err != nil {
		return ProfileStatus{}, err
	}
	if tool != "claude" && tool != "codex" {
		return ProfileStatus{}, errors.New("an account belongs to claude or codex")
	}
	if err := ValidateAccountLabel(label); err != nil {
		return ProfileStatus{}, err
	}
	if m.config.UserStateRoot == "" {
		return ProfileStatus{}, errors.New("accounts require a configured Sessions user state root")
	}
	path := filepath.Join(m.config.UserStateRoot, "profiles", tool, name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return ProfileStatus{}, fmt.Errorf("create account directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return ProfileStatus{}, fmt.Errorf("make account directory private: %w", err)
	}
	sidecar := readAccountSidecar(m.config.UserStateRoot, tool, name)
	if sidecar.Removed {
		// Adding a forgotten name again may reach a home signed into something
		// else now: what an earlier check found stays only as history.
		sidecar.observe(AccountCheck{At: time.Now().UnixMilli(), Outcome: AccountCheckNotChecked}, nil)
	}
	sidecar.Removed = false
	m.accountUsage.invalidate(tool, name)
	if label != "" {
		sidecar.Label = label
	}
	if err := writeAccountSidecar(m.config.UserStateRoot, tool, name, sidecar); err != nil {
		return ProfileStatus{}, err
	}
	info, err := os.Stat(path)
	lastUsed := int64(0)
	if err == nil {
		lastUsed = info.ModTime().UnixMilli()
	}
	status := ProfileStatus{
		Tool: tool, Name: name, Path: path, Label: sidecar.Label,
		SignedIn: profileSignedIn(path, tool),
		Sessions: make([]ProfileSession, 0), LastUsed: lastUsed,
	}
	sidecar.applyTo(&status)
	return status, nil
}

// RenameAccount changes only the nickname a person gave an account. The account
// ID, its provider home, its login and its history stay exactly where they are;
// an empty label clears the nickname. A forgotten account is not renamed back
// onto the list: that is what adding it again is for.
func (m *Manager) RenameAccount(tool, name, label string) (ProfileStatus, error) {
	m.accountMetadataMu.Lock()
	defer m.accountMetadataMu.Unlock()
	if err := state.ValidateProfileName(name); err != nil {
		return ProfileStatus{}, err
	}
	if tool != "claude" && tool != "codex" {
		return ProfileStatus{}, errors.New("an account belongs to claude or codex")
	}
	if err := ValidateAccountLabel(label); err != nil {
		return ProfileStatus{}, err
	}
	if m.config.UserStateRoot == "" {
		return ProfileStatus{}, errors.New("accounts require a configured Sessions user state root")
	}
	path := filepath.Join(m.config.UserStateRoot, "profiles", tool, name)
	info, err := os.Stat(path)
	sidecar := readAccountSidecar(m.config.UserStateRoot, tool, name)
	if err != nil || !info.IsDir() || sidecar.Removed {
		return ProfileStatus{}, fmt.Errorf("unknown account %s/%s; `sessions accounts` lists the accounts on this computer", tool, name)
	}
	sidecar.Label = label
	if err := writeAccountSidecar(m.config.UserStateRoot, tool, name, sidecar); err != nil {
		return ProfileStatus{}, fmt.Errorf("save account nickname: %w", err)
	}
	status := ProfileStatus{
		Tool: tool, Name: name, Path: path, Label: sidecar.Label,
		SignedIn: profileSignedIn(path, tool),
		Sessions: make([]ProfileSession, 0), LastUsed: info.ModTime().UnixMilli(),
	}
	sidecar.applyTo(&status)
	return status, nil
}

// ForgetAccount takes an account off this machine's list without touching the
// provider home behind it. The home holds a real subscription's login and
// history; unregistering is a decision about a list, and deleting it would be a
// decision about someone's account.
func (m *Manager) ForgetAccount(tool, name string) error {
	m.accountMetadataMu.Lock()
	defer m.accountMetadataMu.Unlock()
	if err := state.ValidateProfileName(name); err != nil {
		return err
	}
	if tool != "claude" && tool != "codex" {
		return errors.New("an account belongs to claude or codex")
	}
	if m.config.UserStateRoot == "" {
		return errors.New("accounts require a configured Sessions user state root")
	}
	path := filepath.Join(m.config.UserStateRoot, "profiles", tool, name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("unknown account %s/%s", tool, name)
	}
	sidecar := readAccountSidecar(m.config.UserStateRoot, tool, name)
	sidecar.Removed = true
	m.accountUsage.invalidate(tool, name)
	return writeAccountSidecar(m.config.UserStateRoot, tool, name, sidecar)
}

// AccountHomePath is where a forgotten account's provider home was left, so the
// answer a person is given names the directory they may want to review.
func (m *Manager) AccountHomePath(tool, name string) string {
	if m.config.UserStateRoot == "" {
		return ""
	}
	return filepath.Join(m.config.UserStateRoot, "profiles", tool, name)
}
