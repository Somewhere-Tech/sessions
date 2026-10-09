package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func installAccountPathFixture(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix service PATH and shell fixture")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := filepath.Join(root, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAccountLoginFindsUserInstallWithServicePATH(t *testing.T) {
	fixture := `#!/bin/sh
if [ "$1 $2" = "auth status" ]; then
  if [ -f "$CLAUDE_CONFIG_DIR/fixture-login" ]; then
    echo '{"loggedIn":true,"authMethod":"claude.ai","email":"isolated@example.test","subscriptionType":"max"}'
  else
    echo '{"loggedIn":false}'
    exit 1
  fi
else
  [ "$1 $2 $3" = "auth login --claudeai" ] || exit 2
  [ "$CLAUDE_CONFIG_DIR" -ef "$PWD" ] || exit 3
  [ -z "$CODEX_HOME$ANTHROPIC_AUTH_TOKEN$CLAUDE_CODE_OAUTH_TOKEN" ] || exit 4
  account-login-dependency || exit 5
  echo 'https://claude.com/cai/oauth/authorize?state=fixture'
  read code
  [ "$code" = fixture-code ] || exit 6
  touch "$CLAUDE_CONFIG_DIR/fixture-login"
fi
`
	root := installAccountPathFixture(t, "claude", fixture)
	dependency := filepath.Join(root, ".local", "bin", "account-login-dependency")
	if err := os.WriteFile(dependency, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "do-not-inherit")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "do-not-inherit")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "do-not-inherit")
	manager, _, _ := newWorktreeTestManager(t, root)
	account, err := manager.CreateAccount("claude", "work", "Work")
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.StartAccountLogin("claude", account.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.AccountLogin(first.ID, "", true) })
	waitLoginState(t, manager, first.ID, "waiting")
	if _, err := manager.AccountLogin(first.ID, "fixture-code", false); err != nil {
		t.Fatal(err)
	}
	connected := waitLoginState(t, manager, first.ID, "connected")
	if connected.Identity == nil || connected.Identity.Email != "isolated@example.test" {
		t.Fatalf("account identity not confirmed: %+v", connected)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "fixture-login")); !os.IsNotExist(err) {
		t.Fatal("login wrote to the default account")
	}
}

func TestAccountUsageFindsUserInstallWithServicePATH(t *testing.T) {
	root := installAccountPathFixture(t, "codex", "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > \"$HOME/fixture-account-home\"\nexit 1\n")
	home := filepath.Join(root, "private-account")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	usage := readProviderUsage(ctx, "codex", home)
	if strings.Contains(usage.Message, "not installed") {
		t.Fatalf("installed CLI was not discovered: %+v", usage)
	}
	data, err := os.ReadFile(filepath.Join(root, "fixture-account-home"))
	if err != nil || string(data) != home {
		t.Fatalf("usage helper did not use the selected account: %q, %v", data, err)
	}
}

func TestAccountLoginFailureExplainsMissingProviderWithoutLeakingErrors(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		message := accountLoginFailureMessage(tool, errAccountProviderUnavailable)
		if !strings.Contains(message, "could not find") || !strings.Contains(message, "try Sign in again") {
			t.Fatalf("missing provider has no recovery instruction: %q", message)
		}
	}
	if message := accountLoginFailureMessage("claude", errors.New("secret OAuth state")); strings.Contains(message, "secret") {
		t.Fatal("provider failure details were published")
	}
}
