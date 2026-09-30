package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

const (
	remoteSlowInterval    = 5 * time.Minute
	remoteNetworkInterval = 5 * time.Second
)

type RemoteState struct {
	Auto              bool   `json:"auto"`
	Present           bool   `json:"present"`
	SignedIn          bool   `json:"signedIn"`
	CurrentDNSName    string `json:"-"`
	ServedDNSName     string `json:"-"`
	Endpoint          string `json:"endpoint,omitempty"`
	TailnetIPEndpoint string `json:"tailnetIpEndpoint,omitempty"`
	Enabled           bool   `json:"enabled"`
	Preview           bool   `json:"preview,omitempty"`
	LastError         string `json:"-"`
}

type tailscaleClient interface {
	Status(context.Context) (tailscale.Status, error)
	ServedEndpoint(context.Context, string, string) (string, error)
	Enable(context.Context, string) error
	Disable(context.Context) error
}

type remoteManager struct {
	mu           sync.RWMutex
	checkMu      sync.Mutex
	config       state.Config
	settingsPath string
	preview      bool
	current      RemoteState
	newClient    func() (tailscaleClient, error)
	logf         func(string, ...any)
	onEndpoints  func(string, string) error
	lastLog      string
}

func newRemoteManager(config state.Config, settingsPath string) *remoteManager {
	return &remoteManager{
		config: config, settingsPath: settingsPath,
		current: RemoteState{Auto: true},
		newClient: func() (tailscaleClient, error) {
			client, err := tailscale.NewClient()
			return client, err
		},
	}
}

func (s *Server) tailscaleHealth() map[string]any {
	remoteState := s.remote.state()
	return map[string]any{
		"present": remoteState.Present, "signedIn": remoteState.SignedIn,
		"remoteEndpoint": remoteState.Endpoint, "tailnetIpEndpoint": remoteState.TailnetIPEndpoint,
		"auto": remoteState.Auto, "enabled": remoteState.Enabled, "preview": remoteState.Preview,
	}
}

func (s *Server) tailscaleDeepHealth() map[string]any {
	health := s.tailscaleHealth()
	remoteState := s.remote.state()
	if remoteState.CurrentDNSName != "" {
		health["currentDNSName"] = remoteState.CurrentDNSName
	}
	if remoteState.ServedDNSName != "" {
		health["servedDNSName"] = remoteState.ServedDNSName
	}
	return health
}

func (m *remoteManager) state() RemoteState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *remoteManager) setPreview(preview bool) {
	m.mu.Lock()
	m.preview = preview
	m.mu.Unlock()
}

func (m *remoteManager) run(ctx context.Context, logf func(string, ...any)) {
	m.mu.Lock()
	m.logf = logf
	m.mu.Unlock()
	slow := time.NewTicker(remoteSlowInterval)
	network := time.NewTicker(remoteNetworkInterval)
	defer slow.Stop()
	defer network.Stop()
	m.check(ctx)
	signature := networkSignature()
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			m.check(ctx)
		case <-network.C:
			next := networkSignature()
			if next != signature {
				signature = next
				m.check(ctx)
			}
		}
	}
}

func (m *remoteManager) check(parent context.Context) {
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	settings, err := state.LoadSettings(m.settingsPath)
	if err != nil {
		m.finish(RemoteState{Auto: true}, fmt.Errorf("load remote setting: %w", err))
		return
	}
	m.mu.RLock()
	preview := m.preview
	m.mu.RUnlock()
	current := RemoteState{Auto: settings.EffectiveRemote().Auto, Preview: preview}
	client, err := m.newClient()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			m.finish(current, nil)
			return
		}
		m.finish(current, err)
		return
	}
	status, err := client.Status(ctx)
	current.Present = true
	if err != nil {
		m.finish(current, err)
		return
	}
	current.SignedIn = status.SignedIn
	if !status.SignedIn {
		m.finish(current, nil)
		return
	}
	current.CurrentDNSName = status.DNSName
	target := remoteDaemonTarget(m.config)
	if !current.Auto {
		served, _ := client.ServedEndpoint(ctx, target, status.DNSName)
		current.ServedDNSName = tailscale.EndpointHost(served)
		if served != "" && !current.Preview {
			if err := client.Disable(ctx); err != nil {
				m.finish(current, err)
				return
			}
			current.ServedDNSName = ""
		}
		m.finish(current, nil)
		return
	}
	current, err = m.checkAutomatic(ctx, client, status, target, current)
	m.finish(current, err)
}

func (m *remoteManager) checkAutomatic(
	ctx context.Context, client tailscaleClient, status tailscale.Status, target string, current RemoteState,
) (RemoteState, error) {
	if current.Auto && status.TailnetIPv4 != "" {
		current.TailnetIPEndpoint = "http://" + net.JoinHostPort(status.TailnetIPv4, fmt.Sprint(m.config.Port))
	}
	served, serveErr := client.ServedEndpoint(ctx, target, status.DNSName)
	current.ServedDNSName = tailscale.EndpointHost(served)
	if served != "" && current.ServedDNSName == status.DNSName {
		current.Endpoint, current.Enabled = served, true
	}
	if served != "" && current.ServedDNSName != status.DNSName && status.DNSName != "" && !current.Preview {
		m.logTailnetNameChange(status.DNSName)
		if err := client.Enable(ctx, target); err != nil {
			return current, fmt.Errorf("reconfigure Tailscale Serve: %w", err)
		}
		served, serveErr = client.ServedEndpoint(ctx, target, status.DNSName)
		current.ServedDNSName = tailscale.EndpointHost(served)
		if current.ServedDNSName == status.DNSName {
			current.Endpoint, current.Enabled = served, true
		}
		return current, serveErr
	}
	if served == "" {
		if current.Preview {
			current.Endpoint = status.Endpoint
			return current, serveErr
		}
		if err := client.Enable(ctx, target); err != nil {
			return current, fmt.Errorf("enable Tailscale Serve: %w", err)
		}
		served, serveErr = client.ServedEndpoint(ctx, target, status.DNSName)
		current.ServedDNSName = tailscale.EndpointHost(served)
		if current.ServedDNSName == status.DNSName {
			current.Endpoint, current.Enabled = served, served != ""
		}
	}
	if current.Endpoint == "" && current.Preview {
		current.Endpoint = status.Endpoint
	}
	return current, serveErr
}

func (m *remoteManager) finish(next RemoteState, err error) {
	if !next.Preview {
		next.Enabled = next.Endpoint != "" || next.TailnetIPEndpoint != ""
	}
	if err != nil {
		next.LastError = err.Error()
	}
	m.mu.Lock()
	m.current = next
	m.mu.Unlock()
	if m.onEndpoints != nil {
		if endpointErr := m.onEndpoints(next.Endpoint, next.TailnetIPEndpoint); endpointErr != nil {
			next.LastError = endpointErr.Error()
			if next.Endpoint == "" {
				next.Enabled = false
			}
			m.mu.Lock()
			m.current = next
			m.mu.Unlock()
		}
	}
	m.logOnce(next.LastError)
}

func (m *remoteManager) logOnce(detail string) {
	m.mu.Lock()
	logf, previous := m.logf, m.lastLog
	m.lastLog = detail
	m.mu.Unlock()
	if detail == "" || logf == nil {
		return
	}
	if detail != previous {
		logf("sessionsd: automatic Tailscale reachability unavailable: %s", detail)
	}
}

func (m *remoteManager) logTailnetNameChange(name string) {
	m.mu.RLock()
	logf := m.logf
	m.mu.RUnlock()
	if logf != nil {
		logf("tailnet name changed: re-serving as %s", name)
	}
}

func (m *remoteManager) setAuto(ctx context.Context, enabled bool) (RemoteState, error) {
	if err := state.UpdateSettings(m.settingsPath, func(settings *state.Settings) error {
		settings.Remote = &state.RemoteSettings{Auto: enabled}
		return nil
	}); err != nil {
		return RemoteState{}, err
	}
	m.mu.RLock()
	preview := m.preview
	m.mu.RUnlock()
	if !enabled && !preview {
		client, err := m.newClient()
		if err == nil {
			served, _ := client.ServedEndpoint(ctx, remoteDaemonTarget(m.config), "")
			if served != "" {
				if err := client.Disable(ctx); err != nil {
					return RemoteState{}, err
				}
			}
		}
	}
	m.check(ctx)
	return m.state(), nil
}

func remoteDaemonTarget(config state.Config) string {
	host := strings.Trim(strings.TrimSpace(config.Host), "[]")
	return "http://" + net.JoinHostPort(host, fmt.Sprint(config.Port))
}

func networkSignature() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.String())
	}
	sort.Strings(values)
	return strings.Join(values, "|")
}

func (s *Server) StartRemote(ctx context.Context, logf func(string, ...any)) {
	go s.remote.run(ctx, logf)
}

func (s *Server) SetRemotePreview(preview bool) {
	s.remote.setPreview(preview)
}

func (s *Server) handleRemoteRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/remote" {
		return false
	}
	if request.Method == http.MethodGet {
		s.sendJSON(response, http.StatusOK, s.remote.state(), corsOrigin)
		return true
	}
	if request.Method != http.MethodPut {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if !s.requireLocalPrincipal(response, request, corsOrigin, "automatic Tailscale reachability") {
		return true
	}
	var body struct {
		Auto *bool `json:"auto"`
	}
	if err := readJSON(request, &body); err != nil || body.Auto == nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "auto must be true or false"}, corsOrigin)
		return true
	}
	current, err := s.remote.setAuto(request.Context(), *body.Auto)
	if err != nil {
		s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, current, corsOrigin)
	return true
}
