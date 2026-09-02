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

// nativeShellOrigins are the origins the Tauri webview reports. macOS uses the
// custom tauri:// scheme, whose hostname happens to be "localhost"; Windows and
// Android report tauri.localhost, which is not a loopback hostname and so is not
// matched by the generic checks below. trustedAmbientWriteOrigin already grants
// these write authority, so omitting them here refused the native client's
// WebSocket upgrade outright on those platforms.
var nativeShellOrigins = map[string]struct{}{
	"tauri://localhost":       {},
	"http://tauri.localhost":  {},
	"https://tauri.localhost": {},
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
	if _, allowed := nativeShellOrigins[normalized]; allowed {
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
	if _, native := nativeShellOrigins[normalized]; native {
		return true
	}
	switch normalized {
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

// trustedRequestHost rejects DNS-rebinding requests before any route can use
// loopback trust. A browser can resolve an attacker-controlled hostname to
// 127.0.0.1 and send a perfectly ordinary GET with that hostname in Host; the
// peer address alone therefore cannot prove the request was addressed to
// Sessions. Every accepted host names the configured listener itself and uses
// its bound port.
func trustedRequestHost(value, bindHost string, bindPort int, additionalHosts ...string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		// A missing port is only an exact description of the listener when it
		// is using HTTP's default port. Malformed host:port values fail closed.
		if strings.Contains(value, ":") || bindPort != 80 {
			return false
		}
		host = value
		portText = "80"
	}
	port, err := strconv.Atoi(portText)
	if err != nil || (bindPort != 0 && port != bindPort) {
		return false
	}
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.EqualFold(host, strings.Trim(bindHost, "[]")) {
		return true
	}
	for _, additional := range additionalHosts {
		if additional != "" && strings.EqualFold(host, strings.Trim(additional, "[]")) {
			return true
		}
	}
	return false
}

func requestHostIdentifiesListener(request *http.Request, bindHost string, bindPort int, additionalHosts ...string) bool {
	// HTTP/1.0 permits an empty Host. It carries no attacker-controlled DNS
	// name to rebind, so keep it only for a direct loopback peer. Browsers and
	// every HTTP/1.1 client send Host and take the strict path below.
	if strings.TrimSpace(request.Host) == "" {
		return isLoopbackPeer(request)
	}
	if trustedRequestHost(request.Host, bindHost, bindPort, additionalHosts...) {
		return true
	}
	// Test servers and embedders may bind an ephemeral port instead of the
	// configured production port. The address on the accepted connection is
	// authoritative and cannot be supplied by the remote caller, so an exact
	// Host match against it is safe and keeps the handler independently usable.
	local, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil {
		return false
	}
	host, portText, err := net.SplitHostPort(local.String())
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return false
	}
	return trustedRequestHost(request.Host, host, port)
}

// websocketWritesAllowed reports whether a `/ws` upgrade may carry the frames
// that reach a live runner: `input`, `submit`, `resize`, and the raw binary /
// non-JSON text frames of single-session mode.
//
// An upgrade is a GET, so the state-changing-method guard in ServeHTTP never
// fires for it, and a loopback peer is authorized() without a credential.
// allowedOrigin then admits any origin whose hostname is loopback with no port
// comparison, so without this check a page on an arbitrary localhost port could
// open ws://127.0.0.1:<port>/ws?mux=1 with no credential and type into the
// user's sessions. That is exactly the ambient authority
// trustedAmbientWriteOrigin denies over HTTP.
//
// Reading stays governed by allowedOrigin so the upgrade itself keeps its
// current outcome for every pinned origin; only write authority narrows.
func (s *Server) websocketWritesAllowed(request *http.Request) (bool, error) {
	origin := request.Header.Get("Origin")
	if origin == "" {
		// Not a browser upgrade. The CLI, native shells, and runner tooling
		// send no Origin, so there is no ambient page authority to contain;
		// authorized() already decided this request.
		return true, nil
	}
	if trustedAmbientWriteOrigin(origin, s.config.Host, s.config.Port, s.lan.activeHost()) {
		return true, nil
	}
	return s.presentedCredential(request)
}

// presentedCredential reports whether the request carried a token that actually
// verifies. Mere presence is never enough: a browser cannot set headers on a
// WebSocket at all, so `?token=` is a channel a hostile page can use too. Only
// a verifying token distinguishes a real client from ambient browser
// authority.
func (s *Server) presentedCredential(request *http.Request) (bool, error) {
	expected, err := s.tokens.token()
	if err != nil {
		return false, err
	}
	candidates := make([]string, 0, 2)
	if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
		candidates = append(candidates, strings.TrimPrefix(authorization, "Bearer "))
	}
	if provided := request.URL.Query().Get("token"); provided != "" {
		candidates = append(candidates, provided)
	}
	for _, candidate := range candidates {
		if tokenEqual(candidate, expected) {
			return true, nil
		}
		_, authorized, err := s.pair.devices.authenticate(candidate)
		if err != nil {
			return false, err
		}
		if authorized {
			return true, nil
		}
	}
	return false, nil
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
