package codexapp

import (
	"context"
	"encoding/json"
)

type accountLoginCompletion struct {
	LoginID string `json:"loginId"`
	Success bool   `json:"success"`
}

func (c *Client) recordAccountLoginCompletion(params json.RawMessage) {
	var result accountLoginCompletion
	if json.Unmarshal(params, &result) != nil || result.LoginID == "" {
		return
	}
	c.mu.Lock()
	c.accountLogin = &result
	c.mu.Unlock()
}

// AccountLoginFailed reports a provider rejection without exposing its raw
// error text, which may contain authentication details. Success still requires
// reading the actual account identity rather than trusting a notification.
func (c *Client) AccountLoginFailed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountLogin != nil && c.accountLogin.LoginID == id && !c.accountLogin.Success
}

// NewAccountClient owns a private stdio process, never the user's shared daemon.
// The caller supplies a profile-scoped environment and closes it after login.
func NewAccountClient(ctx context.Context, executable string, env []string, directory string) (*Client, error) {
	return startAccountProcess(ctx, executable, env, directory)
}

type Account struct {
	Type     string `json:"type"`
	Email    string `json:"email"`
	PlanType string `json:"planType"`
}

type AccountLogin struct {
	LoginID         string `json:"loginId"`
	AuthURL         string `json:"authUrl"`
	VerificationURL string `json:"verificationUrl"`
	UserCode        string `json:"userCode"`
}

func (c *Client) ReadAccount(ctx context.Context) (*Account, error) {
	return c.readAccount(ctx, true)
}

// PeekAccount reads the signed-in identity without asking the provider to
// refresh its token first. A usage read shares its home with running sessions,
// so it leaves token rotation to the provider's own schedule.
func (c *Client) PeekAccount(ctx context.Context) (*Account, error) {
	return c.readAccount(ctx, false)
}

func (c *Client) readAccount(ctx context.Context, refresh bool) (*Account, error) {
	var result struct {
		Account *Account `json:"account"`
	}
	err := c.call(ctx, "account/read", map[string]bool{"refreshToken": refresh}, &result)
	return result.Account, err
}

func (c *Client) StartAccountLogin(ctx context.Context) (AccountLogin, error) {
	var result AccountLogin
	// Device authorization works from another computer or phone as well: no
	// localhost callback is accidentally sent to the machine running the UI.
	err := c.call(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"}, &result)
	return result, err
}
