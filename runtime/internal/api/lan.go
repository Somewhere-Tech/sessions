package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/discovery"
	lanutil "github.com/somewhere-tech/sessions/runtime/internal/lan"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type BonjourState struct {
	Advertised bool   `json:"advertised"`
	Service    string `json:"service"`
	Error      string `json:"error,omitempty"`
}

type LANState struct {
	Enabled bool         `json:"enabled"`
	URL     *string      `json:"url"`
	Bonjour BonjourState `json:"bonjour"`
}

type lanListener struct {
	opMu         sync.Mutex
	mu           sync.Mutex
	config       state.Config
	handler      http.Handler
	pickIP       func() (net.IP, error)
	listen       func(string, string) (net.Listener, error)
	advertise    discovery.AdvertiseFunc
	settingsPath string
	server       *http.Server
	registration discovery.Registration
	bonjourError string
	machineName  string
	machineID    string
	host         string
	url          string
}

type lanRequestContextKey struct{}

func newLANListener(config state.Config, handler http.Handler, identity machineIdentity) *lanListener {
	settingsPath := config.SettingsPath
	if settingsPath == "" {
		root := config.UserStateRoot
		if root == "" {
			root = config.StateRoot
		}
		settingsPath = filepath.Join(root, "settings.json")
	}
	return &lanListener{
		config: config, handler: handler, pickIP: lanutil.PrimaryIPv4,
		listen: net.Listen, advertise: discovery.Advertise, settingsPath: settingsPath,
		machineName: identity.Name, machineID: identity.ID,
	}
}

func (l *lanListener) state() LANState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked()
}

func (l *lanListener) stateLocked() LANState {
	if l.server == nil || l.url == "" {
		return LANState{Bonjour: BonjourState{Service: discovery.ServiceType}}
	}
	url := l.url
	return LANState{
		Enabled: true, URL: &url,
		Bonjour: BonjourState{
			Advertised: l.registration != nil,
			Service:    discovery.ServiceType,
			Error:      l.bonjourError,
		},
	}
}

func (l *lanListener) activeHost() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.host
}

func (l *lanListener) enable(persist bool) (LANState, error) {
	l.opMu.Lock()
	defer l.opMu.Unlock()

	ip, err := l.pickIP()
	if err != nil {
		return l.state(), err
	}
	host := ip.String()
	l.mu.Lock()
	if l.server != nil && l.host == host {
		port := portFromURL(l.url, l.config.Port)
		l.mu.Unlock()
		l.ensureBonjour(ip, port)
		if persist {
			if err := l.persistEnabled(true); err != nil {
				return l.state(), err
			}
		}
		return l.state(), nil
	}
	l.mu.Unlock()

	address := net.JoinHostPort(host, strconv.Itoa(l.config.Port))
	listener, err := l.listen("tcp", address)
	if err != nil {
		return l.state(), fmt.Errorf("could not open the LAN listener at %s: %w; make sure this Mac is still on the network that owns %s and port %d is free, then retry `sessions lan enable`", address, err, host, l.config.Port)
	}
	actualAddress, ok := listener.Addr().(*net.TCPAddr)
	if ok {
		address = net.JoinHostPort(host, strconv.Itoa(actualAddress.Port))
	}
	url := "http://" + address
	if persist {
		if err := l.persistEnabled(true); err != nil {
			_ = listener.Close()
			return l.state(), err
		}
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			requestContext := context.WithValue(request.Context(), lanRequestContextKey{}, true)
			l.handler.ServeHTTP(response, request.WithContext(requestContext))
		}),
		ReadHeaderTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second,
	}
	l.mu.Lock()
	previous := l.server
	previousRegistration := l.registration
	l.server = server
	l.registration = nil
	l.bonjourError = ""
	l.host = host
	l.url = url
	l.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("sessionsd: LAN listener error: %v", err)
			var registration discovery.Registration
			l.mu.Lock()
			if l.server == server {
				l.server = nil
				l.host = ""
				l.url = ""
				registration = l.registration
				l.registration = nil
				l.bonjourError = ""
			}
			l.mu.Unlock()
			if registration != nil {
				_ = registration.Shutdown()
			}
		}
	}()
	l.ensureBonjour(ip, portFromURL(url, l.config.Port))
	if previous != nil {
		_ = previous.Close()
	}
	if previousRegistration != nil {
		_ = previousRegistration.Shutdown()
	}
	return l.state(), nil
}

func (l *lanListener) disable(persist bool) (LANState, error) {
	l.opMu.Lock()
	defer l.opMu.Unlock()
	if persist {
		if err := l.persistEnabled(false); err != nil {
			return l.state(), err
		}
	}
	l.mu.Lock()
	server := l.server
	registration := l.registration
	l.server = nil
	l.registration = nil
	l.bonjourError = ""
	l.host = ""
	l.url = ""
	l.mu.Unlock()
	if server != nil {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return LANState{}, fmt.Errorf("close LAN listener: %w", err)
		}
	}
	if registration != nil {
		if err := registration.Shutdown(); err != nil {
			return LANState{}, fmt.Errorf("stop Bonjour advertisement: %w", err)
		}
	}
	return LANState{Bonjour: BonjourState{Service: discovery.ServiceType}}, nil
}

func (l *lanListener) ensureBonjour(ip net.IP, port int) {
	l.mu.Lock()
	if l.registration != nil {
		l.mu.Unlock()
		return
	}
	server := l.server
	host := l.host
	l.mu.Unlock()

	registration, err := l.advertise(ip, port, l.machineName, l.machineID)
	if err != nil {
		l.mu.Lock()
		if l.server == server && l.host == host {
			l.bonjourError = "Bonjour discovery could not start; retry `sessions lan enable` or use the pairing link."
		}
		l.mu.Unlock()
		log.Printf("sessionsd: Bonjour advertisement unavailable: %v", err)
		return
	}
	l.mu.Lock()
	if l.server != server || l.host != host || l.registration != nil {
		l.mu.Unlock()
		_ = registration.Shutdown()
		return
	}
	l.registration = registration
	l.bonjourError = ""
	l.mu.Unlock()
}

func portFromURL(value string, fallback int) int {
	_, rawPort, err := net.SplitHostPort(strings.TrimPrefix(value, "http://"))
	if err != nil {
		return fallback
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return fallback
	}
	return port
}

func isLANRequest(request *http.Request) bool {
	value, _ := request.Context().Value(lanRequestContextKey{}).(bool)
	return value
}

func (l *lanListener) persistEnabled(enabled bool) error {
	return state.UpdateSettings(l.settingsPath, func(settings *state.Settings) error {
		settings.LAN = enabled
		return nil
	})
}

func (s *Server) RestoreLAN(logf func(string, ...any)) {
	settings, err := state.LoadSettings(s.lan.settingsPath)
	if err != nil {
		logf("sessionsd: could not load LAN setting: %v; continuing without LAN access", err)
		return
	}
	if !settings.LAN {
		return
	}
	current, err := s.lan.enable(false)
	if err != nil {
		logf("sessionsd: LAN access is enabled in settings but could not start: %v; continuing without LAN access", err)
		return
	}
	logf("sessionsd: LAN access listening on %s", *current.URL)
}

func (s *Server) CloseLAN() error {
	_, err := s.lan.disable(false)
	return err
}

func (s *Server) handleLANRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/lan" {
		return false
	}
	switch request.Method {
	case http.MethodGet:
		s.sendJSON(response, http.StatusOK, s.lan.state(), corsOrigin)
	case http.MethodPost:
		// Opening the plaintext-HTTP LAN listener and its Bonjour
		// advertisement is a separate capability from remote API access, and
		// docs/NETWORK_SECURITY.md requires that enabling one never silently
		// enables another. Match /api/pair/ticket, /api/devices, and
		// /api/access/requests: only a principal on this machine may change
		// it. Reading the state stays available to any authorized caller.
		if !s.requireLocalPrincipal(response, request, corsOrigin, "LAN access administration") {
			return true
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if body.Enabled == nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "enabled must be true or false"}, corsOrigin)
			return true
		}
		var (
			current LANState
			err     error
		)
		if *body.Enabled {
			current, err = s.lan.enable(true)
		} else {
			current, err = s.lan.disable(true)
		}
		if err != nil {
			s.sendJSON(response, http.StatusConflict, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, current, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}
