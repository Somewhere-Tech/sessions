package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/agentcall"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

var errAccountProviderUnavailable = errors.New("account provider executable unavailable")

// Do not inherit API keys, ambient provider homes, gateways or auth helpers.
// Sign-in and verification run in the same private home used by the account.
func accountLoginEnvironment(tool, home string) []string {
	var env []string
	// Share GUI/service-safe executable discovery and PATH with agent calls,
	// but retain the narrower account allowlist below. In particular, never
	// inherit the caller's CLAUDE_CONFIG_DIR or CODEX_HOME.
	for _, item := range agentcall.Environment() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "PATH", "HOME", "USER", "USERNAME", "USERPROFILE", "LOGNAME", "SHELL",
			"TMPDIR", "TEMP", "TMP", "SYSTEMROOT", "WINDIR", "APPDATA", "LOCALAPPDATA",
			"LANG", "LC_ALL", "LC_CTYPE", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
			"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS":
			env = append(env, item)
		}
	}
	key := "CODEX_HOME"
	if tool == "claude" {
		key = "CLAUDE_CONFIG_DIR"
	}
	env = append(env, key+"="+home, "NO_COLOR=1")
	// Sessions presents the provider URL on the requesting device. Suppress
	// opening a second browser on a remote host, using a no-op executable.
	if runtime.GOOS != "windows" {
		env = append(env, "BROWSER=/usr/bin/true")
	}
	return env
}

func loginProviderAccount(ctx context.Context, tool, home string, op *accountLoginOperation) (*AccountIdentity, error) {
	executable, err := agentcall.Executable(tool)
	if err != nil {
		return nil, errAccountProviderUnavailable
	}
	env := accountLoginEnvironment(tool, home)
	if tool == "claude" {
		return loginClaudeAccount(ctx, executable, home, env, op)
	}
	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	client, err := codexapp.NewAccountClient(initCtx, executable, env, home)
	cancel()
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if identity, err := readCodexIdentity(ctx, client); err != nil || identity != nil {
		return identity, err
	}
	if err := op.observe(AccountCheckSignedOut); err != nil {
		return nil, err
	}
	loginCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	login, err := client.StartAccountLogin(loginCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	if !allowedAccountLoginURL("codex", login.VerificationURL) || login.UserCode == "" {
		return nil, errors.New("provider did not return a device sign-in link")
	}
	op.update(func(s *AccountLoginStatus) { s.State, s.URL, s.Code = "waiting", login.VerificationURL, login.UserCode })
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if client.AccountLoginFailed(login.LoginID) {
				return nil, errors.New("ChatGPT sign-in was not completed")
			}
			identity, err := readCodexIdentity(ctx, client)
			if err != nil || identity != nil {
				return identity, err
			}
		}
	}
}

func readCodexIdentity(ctx context.Context, client *codexapp.Client) (*AccountIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	account, err := client.ReadAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAccountStatusUnreadable, err)
	}
	return codexAccountIdentity(account, time.Now().UnixMilli())
}

// codexAccountIdentity classifies one account/read answer: no account is
// signed out, a non-ChatGPT login is not a subscription, and a ChatGPT login
// without an email is an answer Sessions cannot verify.
func codexAccountIdentity(account *codexapp.Account, checkedAt int64) (*AccountIdentity, error) {
	switch {
	case account == nil:
		return nil, nil
	case account.Type != "chatgpt":
		return nil, errAccountNotSubscription
	case account.Email == "":
		return nil, fmt.Errorf("%w: ChatGPT reported no account email", errAccountStatusUnreadable)
	}
	return &AccountIdentity{Email: account.Email, Plan: account.PlanType, CheckedAt: checkedAt}, nil
}

func readClaudeIdentity(ctx context.Context, executable, home string, env []string) (*AccountIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "auth", "status", "--json")
	cmd.Env, cmd.Dir = env, home
	output, err := cmd.Output()
	return claudeAccountIdentity(output, err, time.Now().UnixMilli())
}

// claudeAccountIdentity classifies one `auth status --json` answer. Only an
// explicit loggedIn:false is signed out; output that does not parse, or that
// omits loggedIn, establishes nothing about the home.
func claudeAccountIdentity(output []byte, runErr error, checkedAt int64) (*AccountIdentity, error) {
	var status struct {
		LoggedIn         *bool  `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		Email            string `json:"email"`
		OrgName          string `json:"orgName"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if json.Unmarshal(output, &status) != nil || status.LoggedIn == nil {
		return nil, fmt.Errorf("%w: Claude account status did not parse", errAccountStatusUnreadable)
	}
	switch {
	case !*status.LoggedIn:
		return nil, nil
	case runErr != nil || status.AuthMethod == "":
		return nil, fmt.Errorf("%w: Claude did not report how it is signed in", errAccountStatusUnreadable)
	case status.AuthMethod != "claude.ai":
		return nil, errAccountNotSubscription
	case status.Email == "":
		return nil, fmt.Errorf("%w: Claude reported no account email", errAccountStatusUnreadable)
	}
	return &AccountIdentity{Email: status.Email, Plan: status.SubscriptionType, Organization: status.OrgName, CheckedAt: checkedAt}, nil
}

func loginClaudeAccount(ctx context.Context, executable, home string, env []string, op *accountLoginOperation) (*AccountIdentity, error) {
	if identity, err := readClaudeIdentity(ctx, executable, home, env); err != nil || identity != nil {
		return identity, err
	}
	if err := op.observe(AccountCheckSignedOut); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "auth", "login", "--claudeai")
	cmd.Env, cmd.Dir = env, home
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	op.mu.Lock()
	op.input = input
	op.mu.Unlock()
	defer func() { op.mu.Lock(); op.input = nil; op.mu.Unlock() }()
	// Raw provider output is deliberately neither returned nor persisted.
	output := &accountLoginOutput{op: op}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Claude sign-in did not finish")
	}
	identity, err := readClaudeIdentity(ctx, executable, home, env)
	if err == nil && identity == nil {
		err = errors.New("Claude did not confirm an account after login")
	}
	return identity, err
}
