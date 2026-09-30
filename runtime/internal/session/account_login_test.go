package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAccountLoginEnvironmentDoesNotInheritCredentials(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_PROFILE", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SESSIONS_CODEX_APP_SERVER_SOCKET"} {
		t.Setenv(key, "do-not-inherit")
	}
	for _, tool := range []string{"claude", "codex"} {
		env := strings.Join(accountLoginEnvironment(tool, "/isolated"), "\n")
		if strings.Contains(env, "do-not-inherit") || !strings.Contains(env, "=/isolated") {
			t.Fatal("profile environment leaked an ambient login")
		}
	}
}

func TestAccountLoginOutputOnlyPublishesCompleteProviderURLs(t *testing.T) {
	op := &accountLoginOperation{status: AccountLoginStatus{State: "opening"}}
	w := &accountLoginOutput{op: op}
	w.Write([]byte("https://claude.com.attacker.test/oauth/authorize\n"))
	w.Write([]byte("https://claude.com/cai/oauth/authorize?state=fi"))
	if op.snapshot().URL != "" {
		t.Fatal("published untrusted or partial URL")
	}
	w.Write([]byte("xture\nPaste code here > "))
	if got := op.snapshot(); got.State != "waiting" || got.URL != "https://claude.com/cai/oauth/authorize?state=fixture" {
		t.Fatalf("status=%+v", got)
	}
	op.update(func(s *AccountLoginStatus) { s.State = "cancelled" })
	w.Write([]byte("\nhttps://claude.com/cai/oauth/authorize?state=later\n"))
	if op.snapshot().State != "cancelled" {
		t.Fatal("output revived a cancelled login")
	}
}

func TestAccountLoginGeneratedNamesAndClaudeFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := `#!/bin/sh
if [ "$1 $2" = "auth status" ]; then
  if [ -f "$CLAUDE_CONFIG_DIR/fixture-login" ]; then
    echo '{"loggedIn":true,"authMethod":"claude.ai","email":"second@example.test","subscriptionType":"max"}'
  else
    echo '{"loggedIn":false}'
    exit 1
  fi
else
  echo 'https://claude.com/cai/oauth/authorize?state=fixture'
  read code
  [ "$code" = fixture-code ] || exit 1
  touch "$CLAUDE_CONFIG_DIR/fixture-login"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, _, _ := newWorktreeTestManager(t, root)
	account, err := manager.CreateAccount("claude", "", "Work")
	if err != nil || !strings.HasPrefix(account.Name, "acct-") {
		t.Fatalf("generated account=%+v err=%v", account, err)
	}
	first, err := manager.StartAccountLogin("claude", account.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.AccountLogin(first.ID, "", true) })
	repeat, err := manager.StartAccountLogin("claude", account.Name)
	if err != nil || repeat.ID != first.ID {
		t.Fatal("double click launched a second login")
	}
	waitLoginState(t, manager, first.ID, "waiting")
	if _, err := manager.AccountLogin(first.ID, "bad\ncode", false); err == nil {
		t.Fatal("accepted multiline input")
	}
	if _, err := manager.AccountLogin(first.ID, "fixture-code", false); err != nil {
		t.Fatal(err)
	}
	result := waitLoginState(t, manager, first.ID, "connected")
	if result.Identity == nil || result.Identity.Email != "second@example.test" {
		t.Fatalf("identity=%+v", result.Identity)
	}
	stored := accountNamed(t, manager, "claude", account.Name)
	if stored.Identity == nil || stored.Identity.Email != result.Identity.Email {
		t.Fatal("identity not persisted")
	}
	if stored.SignedIn {
		t.Fatal("identity check invented a login-file marker")
	}
}

func waitLoginState(t *testing.T, manager *Manager, id, want string) AccountLoginStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		status, err := manager.AccountLogin(id, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return status
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wanted %s, got %+v", want, status)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
