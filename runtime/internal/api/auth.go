package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

var hostedShellOrigins = map[string]struct{}{
	"https://sessions.somewhere.tech": {},
	"https://sessions.somewhere.site": {},
}

type tokenStore struct {
	path string
	mu   sync.Mutex
}

func (s *tokenStore) token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return tokenstore.ReadOrCreate(s.path)
}

func validToken(value string) bool {
	return tokenstore.Valid(value)
}

func tokenEqual(provided, expected string) bool {
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func isLoopbackPeer(request *http.Request) bool {
	for key := range request.Header {
		if strings.EqualFold(key, "X-Forwarded-For") {
			return false
		}
	}
	return isLoopbackRemote(request)
}

func isLoopbackRemote(request *http.Request) bool {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = strings.Trim(request.RemoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type tailnetIdentity struct {
	Login string
	Name  string
}

// Tailscale Serve removes spoofed identity headers from inbound requests and
// adds its own before proxying to the loopback backend. Trust them only when
// the immediate network peer is loopback; a direct LAN caller must never be
// able to self-assert a tailnet identity.
func tailscaleServeIdentity(request *http.Request) (tailnetIdentity, bool) {
	if !isLoopbackRemote(request) {
		return tailnetIdentity{}, false
	}
	logins := request.Header.Values("Tailscale-User-Login")
	if len(logins) != 1 {
		return tailnetIdentity{}, false
	}
	login := strings.TrimSpace(logins[0])
	if !validIdentityHeader(login, 320) {
		return tailnetIdentity{}, false
	}
	name := strings.TrimSpace(request.Header.Get("Tailscale-User-Name"))
	if name != "" && !validIdentityHeader(name, 160) {
		return tailnetIdentity{}, false
	}
	return tailnetIdentity{Login: login, Name: name}, true
}

func validIdentityHeader(value string, maximum int) bool {
	if value == "" || utf8.RuneCountInString(value) > maximum {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func allowedOrigin(origin, bindHost string, additionalHosts ...string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	normalized := normalizedOrigin(parsed)
	if _, allowed := hostedShellOrigins[normalized]; allowed {
		return true
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "127.0.0.1" || hostname == "localhost" || hostname == "::1" {
		return true
	}
	if strings.EqualFold(hostname, strings.Trim(bindHost, "[]")) {
		return true
	}
	for _, host := range additionalHosts {
		if host != "" && strings.EqualFold(hostname, strings.Trim(host, "[]")) {
			return true
		}
	}
	return false
}

func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// trustedAmbientWriteOrigin is deliberately narrower than allowedOrigin.
// allowedOrigin controls readable CORS responses and includes hosted clients
// that authenticate with a token. This function defines the small set of
// native, development, and daemon-same-origin pages that may write using only
// loopback trust.
func trustedAmbientWriteOrigin(origin, bindHost string, bindPort int, additionalHosts ...string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	normalized := normalizedOrigin(parsed)
	switch normalized {
	case "tauri://localhost", "http://tauri.localhost", "https://tauri.localhost":
		return true
	case "http://localhost:5273", "http://127.0.0.1:5273":
		// The checked-in Tauri development URL. Production uses the native
		// origins above, so no arbitrary localhost port receives ambient trust.
		return true
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if effectiveOriginPort(parsed) != bindPort {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "127.0.0.1" || hostname == "localhost" || hostname == "::1" ||
		strings.EqualFold(hostname, strings.Trim(bindHost, "[]")) {
		return true
	}
	for _, host := range additionalHosts {
		if host != "" && strings.EqualFold(hostname, strings.Trim(host, "[]")) {
			return true
		}
	}
	return false
}

func effectiveOriginPort(parsed *url.URL) int {
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil {
			return 0
		}
		return value
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		return 80
	case "https":
		return 443
	default:
		return 0
	}
}

func normalizedOrigin(parsed *url.URL) string {
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}
