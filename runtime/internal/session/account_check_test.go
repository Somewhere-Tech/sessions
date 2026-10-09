package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

// The fake Claude answers `auth status` from a mode file in its own home, and
// its login waits for a code that never comes, so only a cancel ends it.
const fakeClaudeCheck = `#!/bin/sh
mode=$(cat "$CLAUDE_CONFIG_DIR/fixture-mode" 2>/dev/null)
if [ "$1 $2" = "auth status" ]; then
  case "$mode" in
    alice|bob) echo "{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"email\":\"$mode@example.test\",\"subscriptionType\":\"max\"}" ;;
    apikey) echo '{"loggedIn":true,"authMethod":"apiKey"}' ;;
    garbage) echo 'internal error'; exit 1 ;;
    slow) exec sleep 30 ;;
    *) echo '{"loggedIn":false}'; exit 1 ;;
  esac
else
  echo 'https://claude.com/cai/oauth/authorize?state=fixture'
  read code
  exit 1
fi
`

// installFakeClaude puts an inert fake first and alone on PATH, and proves the
// effective environment cannot reach a real provider binary.
func installFakeClaude(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaudeCheck), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if found, err := exec.LookPath("claude"); err != nil || found != filepath.Join(bin, "claude") {
		t.Fatalf("claude resolves to %q (%v), not the fake", found, err)
	}
	if found, err := exec.LookPath("codex"); err == nil {
		t.Fatalf("a real codex is reachable at %s", found)
	}
	for _, item := range accountLoginEnvironment("claude", root) {
		if strings.HasPrefix(item, "PATH=") && item != "PATH="+os.Getenv("PATH") {
			t.Fatalf("login environment widened PATH: %s", item)
		}
	}
}

func setClaudeMode(t *testing.T, home, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "fixture-mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkAccount(t *testing.T, manager *Manager, tool, name, want string) AccountLoginStatus {
	t.Helper()
	started, err := manager.StartAccountLogin(tool, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.AccountLogin(started.ID, "", true) })
	status := waitLoginState(t, manager, started.ID, want)
	if want != "waiting" {
		waitLoginDone(t, manager, started.ID)
	}
	return status
}

func waitLoginDone(t *testing.T, manager *Manager, id string) {
	t.Helper()
	manager.accountLoginMu.Lock()
	op := manager.accountLogins[id]
	manager.accountLoginMu.Unlock()
	select {
	case <-op.done:
	case <-time.After(4 * time.Second):
		t.Fatal("sign-in helper did not finish")
	}
}

func verifiedAccount(t *testing.T, root string) (*Manager, ProfileStatus) {
	t.Helper()
	installFakeClaude(t, root)
	manager, _, _ := newWorktreeTestManager(t, root)
	account, err := manager.CreateAccount("claude", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	setClaudeMode(t, account.Path, "alice")
	checkAccount(t, manager, "claude", "work", "connected")
	listed := accountNamed(t, manager, "claude", "work")
	if listed.Identity == nil || listed.Identity.Email != "alice@example.test" || listed.LastCheck == nil || listed.LastCheck.Outcome != AccountCheckVerified {
		t.Fatalf("verified account = %+v", listed)
	}
	return manager, listed
}

func assertSuperseded(t *testing.T, listed ProfileStatus, outcome string) {
	t.Helper()
	if listed.Identity != nil {
		t.Fatalf("old identity still presented as current: %+v", listed.Identity)
	}
	if listed.LastCheck == nil || listed.LastCheck.Outcome != outcome {
		t.Fatalf("last check = %+v, want %s", listed.LastCheck, outcome)
	}
	if listed.PreviousIdentity == nil || listed.PreviousIdentity.Email != "alice@example.test" {
		t.Fatalf("previous identity = %+v, want alice kept as history", listed.PreviousIdentity)
	}
}

// A check that finds the home signed out says so at once, and cancelling the
// sign-in it then offers does not bring the old identity back. The provider's
// login file is neither removed nor changed.
func TestSignedOutCheckSupersedesIdentityEvenWhenLoginIsCancelled(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	marker := filepath.Join(account.Path, ".credentials.json")
	if err := os.WriteFile(marker, []byte("provider-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	setClaudeMode(t, account.Path, "signed-out")
	waiting := checkAccount(t, manager, "claude", "work", "waiting")
	assertSuperseded(t, accountNamed(t, manager, "claude", "work"), AccountCheckSignedOut)
	if _, err := manager.AccountLogin(waiting.ID, "", true); err != nil {
		t.Fatal(err)
	}
	waitLoginDone(t, manager, waiting.ID)
	listed := accountNamed(t, manager, "claude", "work")
	assertSuperseded(t, listed, AccountCheckSignedOut)
	if !listed.SignedIn {
		t.Fatal("legacy signed_in stopped reporting a present login file")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "provider-owned" {
		t.Fatalf("provider login file changed: %q %v", raw, err)
	}
}

// The same holds when the sign-in expires rather than being cancelled.
func TestSignedOutCheckSurvivesAnExpiredLogin(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	setClaudeMode(t, account.Path, "signed-out")
	waiting := checkAccount(t, manager, "claude", "work", "waiting")
	manager.accountLoginMu.Lock()
	op := manager.accountLogins[waiting.ID]
	manager.accountLoginMu.Unlock()
	op.cancel() // what the ten-minute deadline does
	waitLoginDone(t, manager, waiting.ID)
	if got := op.snapshot().State; got != "expired" {
		t.Fatalf("state = %s, want expired", got)
	}
	assertSuperseded(t, accountNamed(t, manager, "claude", "work"), AccountCheckSignedOut)
}

func TestAPIKeyCheckIsNotASubscription(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	setClaudeMode(t, account.Path, "apikey")
	failed := checkAccount(t, manager, "claude", "work", "failed")
	if !strings.Contains(failed.Message, "not a subscription") {
		t.Fatalf("message = %q", failed.Message)
	}
	assertSuperseded(t, accountNamed(t, manager, "claude", "work"), AccountCheckNotSubscription)
}

// A status that cannot be read is an unknown, never a sign-out.
func TestUnreadableCheckIsFailedNotSignedOut(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	setClaudeMode(t, account.Path, "garbage")
	checkAccount(t, manager, "claude", "work", "failed")
	assertSuperseded(t, accountNamed(t, manager, "claude", "work"), AccountCheckFailed)
}

// Cancelling a check before the provider answered establishes nothing.
func TestCancelledCheckBeforeAnyAnswerRecordsNothing(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	setClaudeMode(t, account.Path, "slow")
	started, err := manager.StartAccountLogin("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AccountLogin(started.ID, "", true); err != nil {
		t.Fatal(err)
	}
	waitLoginDone(t, manager, started.ID)
	listed := accountNamed(t, manager, "claude", "work")
	if listed.Identity == nil || listed.Identity.Email != "alice@example.test" || listed.LastCheck.Outcome != AccountCheckVerified {
		t.Fatalf("a cancelled check changed the record: %+v", listed)
	}
}

func TestSuccessfulRecheckRestoresTheObservedIdentity(t *testing.T) {
	root := t.TempDir()
	manager, account := verifiedAccount(t, root)
	setClaudeMode(t, account.Path, "garbage")
	checkAccount(t, manager, "claude", "work", "failed")
	setClaudeMode(t, account.Path, "bob")
	connected := checkAccount(t, manager, "claude", "work", "connected")
	listed := accountNamed(t, manager, "claude", "work")
	if listed.Identity == nil || listed.Identity.Email != "bob@example.test" || connected.Identity.Email != "bob@example.test" {
		t.Fatalf("identity = %+v", listed.Identity)
	}
	if listed.LastCheck.Outcome != AccountCheckVerified || listed.LastCheck.At != listed.Identity.CheckedAt || listed.PreviousIdentity != nil {
		t.Fatalf("after a verified recheck: %+v", listed)
	}
}

// Adding a forgotten name again cannot revive what an earlier check found.
func TestReaddingAForgottenAccountDoesNotReviveVerification(t *testing.T) {
	root := t.TempDir()
	manager, _ := verifiedAccount(t, root)
	if err := manager.ForgetAccount("claude", "work"); err != nil {
		t.Fatal(err)
	}
	readded, err := manager.CreateAccount("claude", "work", "Again")
	if err != nil {
		t.Fatal(err)
	}
	assertSuperseded(t, readded, AccountCheckNotChecked)
	assertSuperseded(t, accountNamed(t, manager, "claude", "work"), AccountCheckNotChecked)
	renamed, err := manager.RenameAccount("claude", "work", "Renamed")
	if err != nil {
		t.Fatal(err)
	}
	assertSuperseded(t, renamed, AccountCheckNotChecked)
	// Adding a listed account again is not a re-add and changes nothing.
	setClaudeMode(t, readded.Path, "alice")
	checkAccount(t, manager, "claude", "work", "connected")
	again, err := manager.CreateAccount("claude", "work", "")
	if err != nil || again.Identity == nil || again.LastCheck.Outcome != AccountCheckVerified {
		t.Fatalf("idempotent add = %+v %v", again, err)
	}
}

// An older observation finishing late never overwrites a newer one.
func TestOlderObservationNeverOverwritesANewerOne(t *testing.T) {
	identity := func(email string, at int64) *AccountIdentity {
		return &AccountIdentity{Email: email, CheckedAt: at}
	}
	var sidecar accountSidecar
	if !sidecar.observe(AccountCheck{At: 200, Outcome: AccountCheckVerified}, identity("new@example.test", 200)) {
		t.Fatal("first observation rejected")
	}
	if sidecar.observe(AccountCheck{At: 100, Outcome: AccountCheckSignedOut}, nil) {
		t.Fatal("older sign-out overwrote a newer verification")
	}
	if sidecar.observe(AccountCheck{At: 150, Outcome: AccountCheckVerified}, identity("old@example.test", 150)) {
		t.Fatal("older verification overwrote a newer one")
	}
	if sidecar.observe(AccountCheck{At: 300, Outcome: AccountCheckVerified}, nil) {
		t.Fatal("verified without an identity was recorded")
	}
	if sidecar.Identity.Email != "new@example.test" || sidecar.LastCheck.At != 200 {
		t.Fatalf("sidecar = %+v", sidecar)
	}
	// A legacy sidecar dates from its identity check.
	legacy := accountSidecar{Identity: identity("legacy@example.test", 500)}
	if legacy.observe(AccountCheck{At: 400, Outcome: AccountCheckFailed}, nil) || legacy.Identity == nil {
		t.Fatal("an observation older than a legacy identity superseded it")
	}
	if !legacy.observe(AccountCheck{At: 600, Outcome: AccountCheckFailed}, nil) || legacy.Identity != nil || legacy.PreviousIdentity.Email != "legacy@example.test" {
		t.Fatalf("newer failed check did not make the legacy identity history: %+v", legacy)
	}
	// A re-add applies even when the recorded clock is ahead.
	ahead := accountSidecar{Identity: identity("ahead@example.test", time.Now().Add(time.Hour).UnixMilli())}
	if !ahead.observe(AccountCheck{At: time.Now().UnixMilli(), Outcome: AccountCheckNotChecked}, nil) || ahead.Identity != nil {
		t.Fatal("a future-dated identity survived a re-add")
	}
}

// Helpers completing at once leave the newest observation, whatever order the
// writes land in.
func TestConcurrentChecksKeepTheNewestObservation(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	if _, err := manager.CreateAccount("claude", "work", ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := int64(1); i <= 24; i++ {
		wg.Add(1)
		go func(at int64) {
			defer wg.Done()
			var err error
			if at%2 == 0 {
				err = manager.recordAccountCheck("claude", "work", AccountCheck{At: at, Outcome: AccountCheckVerified}, &AccountIdentity{Email: "even@example.test", CheckedAt: at})
			} else {
				err = manager.recordAccountCheck("claude", "work", AccountCheck{At: at, Outcome: AccountCheckSignedOut}, nil)
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	listed := accountNamed(t, manager, "claude", "work")
	if listed.LastCheck == nil || listed.LastCheck.At != 24 || listed.Identity == nil || listed.Identity.CheckedAt != 24 {
		t.Fatalf("newest observation lost: %+v", listed)
	}
}

func TestLegacySidecarStillDecodesAsVerified(t *testing.T) {
	root := t.TempDir()
	manager, _, _ := newWorktreeTestManager(t, root)
	account, err := manager.CreateAccount("codex", "team", "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"identity":{"email":"legacy@example.test","plan":"team","checked_at":1790000000000},"label":"Team"}`
	if err := os.WriteFile(accountSidecarPath(manager.config.UserStateRoot, "codex", "team"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(account.Path, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed := accountNamed(t, manager, "codex", "team")
	if listed.Identity == nil || listed.Identity.Email != "legacy@example.test" || listed.LastCheck != nil || listed.PreviousIdentity != nil || listed.Label != "Team" || !listed.SignedIn {
		t.Fatalf("legacy listing = %+v", listed)
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "last_check") || strings.Contains(string(encoded), "previous_identity") {
		t.Fatalf("legacy listing grew new keys: %s", encoded)
	}
}

func TestProviderAnswersAreClassifiedWithoutInventingSignOut(t *testing.T) {
	for _, tc := range []struct {
		output, want string
		runErr       error
	}{
		{`{"loggedIn":false}`, AccountCheckSignedOut, errors.New("exit 1")},
		{`{"loggedIn":true,"authMethod":"apiKey"}`, AccountCheckNotSubscription, nil},
		{`{"loggedIn":true,"authMethod":"claude.ai","email":"a@example.test"}`, AccountCheckVerified, nil},
		{`{}`, AccountCheckFailed, nil},
		{`not json`, AccountCheckFailed, errors.New("exit 1")},
		{``, AccountCheckFailed, errors.New("not found")},
		{`{"loggedIn":true}`, AccountCheckFailed, nil},
		{`{"loggedIn":true,"authMethod":"claude.ai","email":"a@example.test"}`, AccountCheckFailed, errors.New("exit 1")},
	} {
		identity, err := claudeAccountIdentity([]byte(tc.output), tc.runErr, 1)
		if got := classifyForTest(identity, err); got != tc.want {
			t.Errorf("claude %q: %s, want %s", tc.output, got, tc.want)
		}
	}
	for _, tc := range []struct {
		account *codexapp.Account
		want    string
	}{
		{nil, AccountCheckSignedOut},
		{&codexapp.Account{Type: "apiKey"}, AccountCheckNotSubscription},
		{&codexapp.Account{Type: "chatgpt"}, AccountCheckFailed},
		{&codexapp.Account{Type: "chatgpt", Email: "a@example.test"}, AccountCheckVerified},
	} {
		identity, err := codexAccountIdentity(tc.account, 1)
		if got := classifyForTest(identity, err); got != tc.want {
			t.Errorf("codex %+v: %s, want %s", tc.account, got, tc.want)
		}
	}
}

func classifyForTest(identity *AccountIdentity, err error) string {
	switch {
	case err == nil && identity != nil:
		return AccountCheckVerified
	case err == nil:
		return AccountCheckSignedOut
	case errors.Is(err, errAccountNotSubscription):
		return AccountCheckNotSubscription
	case errors.Is(err, errAccountStatusUnreadable):
		return AccountCheckFailed
	}
	return "unclassified: " + err.Error()
}

func TestFinishedHelpersRecordOnlyWhatTheProviderEstablished(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	verified := &AccountIdentity{Email: "a@example.test", CheckedAt: 7}
	other := errors.New("Claude sign-in did not finish")
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		observed bool
		identity *AccountIdentity
		err      error
		want     string
	}{
		{"success", live, false, verified, nil, AccountCheckVerified},
		{"not a subscription", live, false, nil, errAccountNotSubscription, AccountCheckNotSubscription},
		{"unreadable", live, true, nil, errAccountStatusUnreadable, AccountCheckFailed},
		{"helper could not start", live, false, nil, errors.New("provider is not installed"), AccountCheckFailed},
		{"login failed after a sign-out answer", live, true, nil, other, ""},
		{"cancelled", stopped, false, nil, errAccountStatusUnreadable, ""},
		{"cancelled after a sign-out answer", stopped, true, nil, context.Canceled, ""},
	} {
		check, ok := finalAccountCheck(tc.ctx, tc.observed, tc.identity, tc.err)
		if got := map[bool]string{true: check.Outcome}[ok]; got != tc.want {
			t.Errorf("%s: recorded %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A provider answer that could not be saved is not acknowledged: the helper
// stops before any sign-in, says the account list may be behind and how to
// recover, and nothing weaker is written over it afterwards. Inert recorder:
// no provider, runner or daemon.
func TestUnsavedObservationIsNotAcknowledged(t *testing.T) {
	writes := 0
	diskFull := errors.New("write profiles/claude/work.account.json.tmp: no space left on device")
	op := &accountLoginOperation{record: func(AccountCheck, *AccountIdentity) error {
		writes++
		return diskFull
	}}
	err := op.observe(AccountCheckSignedOut)
	var notSaved *accountCheckNotSavedError
	if !errors.As(err, &notSaved) || notSaved.outcome != AccountCheckSignedOut || !notSaved.beforeLogin || !errors.Is(err, diskFull) || op.observed || writes != 1 {
		t.Fatalf("observe err=%v observed=%v writes=%d", err, op.observed, writes)
	}
	if check, recorded := finalAccountCheck(context.Background(), op.observed, nil, err); recorded {
		t.Fatalf("an unsaved answer was replaced by %+v", check)
	}
	state, message := accountLoginOutcome(context.Background(), err)
	for _, want := range []string{"could not save", "no space left on device", "Sign-in was not started", "check the account again"} {
		if state != "failed" || !strings.Contains(message, want) {
			t.Fatalf("state=%s message=%q lacks %q", state, message, want)
		}
	}
	// Even once the helper has expired, the unsaved answer is what it reports.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if state, message := accountLoginOutcome(stopped, err); state != "failed" || !strings.Contains(message, "could not save") {
		t.Fatalf("expired helper hid the unsaved check: %s %q", state, message)
	}
	// A failed read that could not be saved is still not called a sign-out.
	unknown := &accountCheckNotSavedError{outcome: AccountCheckFailed, err: diskFull}
	if _, message := accountLoginOutcome(context.Background(), unknown); strings.Contains(message, "no sign-in") || !strings.Contains(message, "could not read who is signed in") {
		t.Fatalf("unsaved failed check: %q", message)
	}
}

// The final save comes after the provider may already have completed a
// sign-in, so a failure there reports the confirmed sign-in, says only that
// Sessions could not save the check, and claims nothing about the provider's
// sign-in being unchanged or not started. Inert: no provider or files.
func TestFinalVerifiedSaveFailureDoesNotDenyTheSignIn(t *testing.T) {
	diskFull := errors.New("no space left on device")
	verified := &AccountIdentity{Email: "alice@example.test", CheckedAt: 7}
	op := &accountLoginOperation{observed: true}
	check, ok := finalAccountCheck(context.Background(), op.observed, verified, nil)
	if !ok || check.Outcome != AccountCheckVerified {
		t.Fatalf("final check = %+v %v", check, ok)
	}
	// What runAccountLogin builds when that save fails.
	err := error(&accountCheckNotSavedError{outcome: check.Outcome, err: diskFull})
	state, message := accountLoginOutcome(context.Background(), err)
	if state != "failed" {
		t.Fatalf("an unsaved verification was reported as %s", state)
	}
	for _, want := range []string{"confirmed the sign-in", "could not save this check", "no space left on device", "check the account again", "does not repeat a sign-in or sign out"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	for _, claim := range []string{"not started", "not changed", "no sign-in", "signed out"} {
		if strings.Contains(message, claim) {
			t.Fatalf("message %q claims %q after a sign-in the provider may have completed", message, claim)
		}
	}
	// Nothing weaker is written over it afterwards.
	if _, recorded := finalAccountCheck(context.Background(), true, nil, err); recorded {
		t.Fatal("an unsaved verification was replaced")
	}
	// Only the answer saved before any sign-in says the sign-in was not started.
	early := &accountCheckNotSavedError{outcome: AccountCheckSignedOut, beforeLogin: true, err: diskFull}
	if _, message := accountLoginOutcome(context.Background(), early); !strings.Contains(message, "Sign-in was not started") {
		t.Fatalf("pre-login failure: %q", message)
	}
}

// A sidecar write that fails leaves the saved check exactly as it was, and
// the next write succeeds once the cause is gone. A directory where the
// temporary file belongs stands in for a full disk.
func TestFailedSidecarWriteKeepsTheSavedCheck(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{config: testConfig(root)}
	alice := &AccountIdentity{Email: "alice@example.test", CheckedAt: 100}
	if err := manager.recordAccountCheck("claude", "work", AccountCheck{At: 100, Outcome: AccountCheckVerified}, alice); err != nil {
		t.Fatal(err)
	}
	blocker := accountSidecarPath(manager.config.UserStateRoot, "claude", "work") + ".tmp"
	if err := os.MkdirAll(filepath.Join(blocker, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.recordAccountCheck("claude", "work", AccountCheck{At: 200, Outcome: AccountCheckSignedOut}, nil); err == nil {
		t.Fatal("a failed sidecar write was reported as saved")
	}
	saved := readAccountSidecar(manager.config.UserStateRoot, "claude", "work")
	if saved.Identity == nil || saved.Identity.Email != "alice@example.test" || saved.LastCheck.Outcome != AccountCheckVerified {
		t.Fatalf("failed write changed the saved check: %+v", saved)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if err := manager.recordAccountCheck("claude", "work", AccountCheck{At: 200, Outcome: AccountCheckSignedOut}, nil); err != nil {
		t.Fatal(err)
	}
	saved = readAccountSidecar(manager.config.UserStateRoot, "claude", "work")
	if saved.Identity != nil || saved.LastCheck.Outcome != AccountCheckSignedOut || saved.PreviousIdentity.Email != "alice@example.test" {
		t.Fatalf("retry after the failure: %+v", saved)
	}
}
