package fleetaccount

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const HeartbeatInterval = 5 * time.Minute
const DefaultBaseURL = "https://sessions-fleet.somewhere.site"

type Options struct {
	BaseURL       string
	AccountPath   string
	KeyPath       string
	MachineID     string
	MachineName   string
	DaemonVersion string
	Endpoints     func() Endpoints
	HTTPClient    *http.Client
	Now           func() time.Time
}

type Manager struct {
	state       *store
	keys        *keyStore
	cloud       *cloudClient
	machineID   string
	machineName string
	version     string
	endpoints   func() Endpoints
	now         func() time.Time
}

func New(options Options) (*Manager, error) {
	if options.AccountPath == "" || options.KeyPath == "" || options.MachineID == "" {
		return nil, errors.New("fleet account requires state paths and a machine identity")
	}
	state := &store{path: options.AccountPath}
	if strings.TrimSpace(options.BaseURL) == "" {
		options.BaseURL = DefaultBaseURL
	}
	cloud, err := newCloudClient(options.BaseURL, state, options.HTTPClient)
	if err != nil {
		return nil, err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if options.Endpoints == nil {
		options.Endpoints = func() Endpoints { return Endpoints{} }
	}
	cloud.now = now
	return &Manager{
		state: state, keys: &keyStore{path: options.KeyPath}, cloud: cloud,
		machineID: options.MachineID, machineName: options.MachineName,
		version: options.DaemonVersion, endpoints: options.Endpoints, now: now,
	}, nil
}

func (m *Manager) RequestMagicLink(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return errors.New("enter a valid email address")
	}
	_, err := m.cloud.request(ctx, http.MethodPost, "/api/auth-token/magic-link", map[string]string{"email": email}, false, "", nil)
	return err
}

func (m *Manager) VerifyMagicLink(ctx context.Context, token string) (Status, error) {
	var response struct {
		User         User   `json:"user"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		SessionToken string `json:"session_token"`
	}
	body, err := m.cloud.request(ctx, http.MethodPost, "/api/auth-token/magic-link/verify", map[string]string{"token": normalizeMagicToken(token)}, false, "", nil)
	if err != nil {
		return Status{}, err
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Token == "" || response.RefreshToken == "" {
		return Status{}, errors.New("Somewhere sign-in returned an invalid token pair")
	}
	err = m.state.update(func(state *persistedState) {
		state.Tokens = TokenPair{response.Token, response.RefreshToken, response.SessionToken}
		state.User = response.User
	})
	if err != nil {
		return Status{}, err
	}
	_ = m.Register(ctx)
	return m.Status()
}

func (m *Manager) Logout(ctx context.Context) error {
	state, err := m.state.load()
	if err != nil {
		return err
	}
	if !state.signedIn() {
		return nil
	}
	key, _, err := m.keys.loadOrCreate()
	if err != nil {
		return err
	}
	_, err = m.cloud.request(ctx, http.MethodDelete, "/api/machines/"+m.machineID, nil, true, m.machineID, key)
	if err != nil {
		return err
	}
	_, err = m.cloud.request(ctx, http.MethodPost, "/api/auth-token/logout", map[string]string{
		"session_token": state.Tokens.SessionToken,
	}, true, "", nil)
	if err != nil {
		return err
	}
	return m.state.clear()
}

func (m *Manager) PublicKey() (string, error) {
	_, public, err := m.keys.loadOrCreate()
	return public, err
}

func (m *Manager) Status() (Status, error) {
	state, err := m.state.load()
	if err != nil {
		return Status{}, err
	}
	status := Status{
		SignedIn: state.signedIn(), LastRegistrationAt: state.LastRegistrationAt,
		LastRegistrationError: state.LastRegistrationError, LastHeartbeatAt: state.LastHeartbeatAt,
	}
	if status.SignedIn {
		user := state.User
		status.User = &user
		status.MachinePublicKey, err = m.PublicKey()
	}
	return status, err
}

func (m *Manager) Health() map[string]any {
	status, err := m.Status()
	if err != nil {
		return map[string]any{"signedIn": false, "error": err.Error()}
	}
	result := map[string]any{"signedIn": status.SignedIn}
	if status.LastRegistrationAt != "" {
		result["lastRegistrationAt"] = status.LastRegistrationAt
	}
	if status.LastRegistrationError != "" {
		result["lastRegistrationError"] = status.LastRegistrationError
	}
	return result
}

func (m *Manager) Register(ctx context.Context) error {
	state, err := m.state.load()
	if err != nil || !state.signedIn() {
		return err
	}
	key, public, err := m.keys.loadOrCreate()
	if err != nil {
		return err
	}
	payload := Registration{
		MachineID: m.machineID, Name: m.machineName, MachinePublicKey: public,
		EndpointsJSON: m.endpoints(), DaemonVersion: m.version,
	}
	_, err = m.cloud.request(ctx, http.MethodPost, "/api/machines/register", payload, true, m.machineID, key)
	m.recordRegistration(err)
	return err
}

func (m *Manager) recordRegistration(registrationErr error) {
	_ = m.state.update(func(state *persistedState) {
		if registrationErr == nil {
			state.LastRegistrationAt = registrationTime(m.now())
			state.LastRegistrationError = ""
			return
		}
		state.LastRegistrationError = registrationErr.Error()
	})
}

func (m *Manager) Heartbeat(ctx context.Context) error {
	state, err := m.state.load()
	if err != nil || !state.signedIn() {
		return err
	}
	key, _, err := m.keys.loadOrCreate()
	if err != nil {
		return err
	}
	_, err = m.cloud.request(ctx, http.MethodPost, "/api/machines/heartbeat", map[string]string{
		"machine_id": m.machineID,
	}, true, m.machineID, key)
	if err == nil {
		_ = m.state.update(func(state *persistedState) { state.LastHeartbeatAt = registrationTime(m.now()) })
	}
	return err
}

func (m *Manager) Start(ctx context.Context, logf func(string, ...any)) {
	go m.run(ctx, logf)
}

func (m *Manager) run(ctx context.Context, logf func(string, ...any)) {
	if err := m.Register(ctx); err != nil {
		logf("sessionsd: fleet registration unavailable: %v", err)
	}
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Register(ctx); err != nil {
				logf("sessionsd: fleet registration refresh unavailable: %v", err)
			}
			if err := m.Heartbeat(ctx); err != nil {
				logf("sessionsd: fleet heartbeat unavailable: %v", err)
			}
		}
	}
}

func normalizeMagicToken(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err == nil && parsed.IsAbs() {
		if token := strings.TrimSpace(parsed.Query().Get("token")); token != "" {
			return token
		}
	}
	return value
}
