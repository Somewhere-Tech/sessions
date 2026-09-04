package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

const defaultIdleTimeout = 2 * time.Minute

type ServerOptions struct {
	Authorizer  Authorizer
	IdleTimeout time.Duration
	Logger      *log.Logger
	Now         func() time.Time
}

type Server struct {
	authorizer Authorizer
	idle       time.Duration
	logger     *log.Logger
	now        func() time.Time
	machinesMu sync.RWMutex
	machines   map[string]*tunnel
}

func NewServer(options ServerOptions) *Server {
	idle := options.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Server{authorizer: options.Authorizer, idle: idle, logger: logger, now: now, machines: make(map[string]*tunnel)}
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/healthz" && request.Method == http.MethodGet:
		s.serveHealth(response)
	case request.URL.Path == "/connect":
		s.serveConnect(response, request)
	case strings.HasPrefix(request.URL.Path, "/m/"):
		s.serveMachine(response, request)
	default:
		http.NotFound(response, request)
	}
}

func (s *Server) serveHealth(response http.ResponseWriter) {
	s.machinesMu.RLock()
	connected := len(s.machines)
	s.machinesMu.RUnlock()
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "name": "sessions-relay", "machines": connected})
}

func (s *Server) serveConnect(response http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(response, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	connection.SetReadLimit(MaxPayload + 1024)
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	machineID, err := s.authenticate(ctx, connection)
	if err != nil {
		cancel()
		s.logger.Printf("event=relay_auth_denied remote=%q error=%q", request.RemoteAddr, err)
		_ = connection.Close(websocket.StatusPolicyViolation, "machine authentication failed")
		return
	}
	if err := writeJSONMessage(ctx, connection, map[string]bool{"ok": true}); err != nil {
		cancel()
		_ = connection.Close(websocket.StatusInternalError, "authentication acknowledgement failed")
		return
	}
	cancel()
	tunnel := newTunnel(request.Context(), connection)
	s.register(machineID, tunnel)
	s.logger.Printf("event=relay_machine_connected machine_id=%q", machineID)
	err = tunnel.readLoop(nil)
	s.unregister(machineID, tunnel)
	s.logger.Printf("event=relay_machine_disconnected machine_id=%q error=%q", machineID, closeReason(err))
}

func (s *Server) authenticate(ctx context.Context, connection *websocket.Conn) (string, error) {
	challenge, err := s.challenge()
	if err != nil {
		return "", err
	}
	if err := writeJSONMessage(ctx, connection, challenge); err != nil {
		return "", err
	}
	var response relayauth.Response
	if err := readJSONMessage(ctx, connection, &response); err != nil {
		return "", err
	}
	if !validMachineID(response.MachineID) || !relayauth.Verify(response, challenge) {
		return "", errors.New("invalid machine challenge signature")
	}
	if s.authorizer == nil {
		return "", errors.New("relay has no machine-key authorizer")
	}
	if err := s.authorizer.Authorize(ctx, response); err != nil {
		return "", err
	}
	return response.MachineID, nil
}

func (s *Server) challenge() (relayauth.Challenge, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return relayauth.Challenge{}, err
	}
	return relayauth.Challenge{
		Timestamp: strconv.FormatInt(s.now().UTC().Unix(), 10),
		Nonce:     base64.RawURLEncoding.EncodeToString(random),
	}, nil
}

func (s *Server) register(machineID string, current *tunnel) {
	s.machinesMu.Lock()
	previous := s.machines[machineID]
	s.machines[machineID] = current
	s.machinesMu.Unlock()
	if previous != nil {
		previous.shutdown()
		_ = previous.connection.Close(websocket.StatusServiceRestart, "machine reconnected")
	}
}

func (s *Server) unregister(machineID string, current *tunnel) {
	s.machinesMu.Lock()
	if s.machines[machineID] == current {
		delete(s.machines, machineID)
	}
	s.machinesMu.Unlock()
}

func (s *Server) serveMachine(response http.ResponseWriter, request *http.Request) {
	machineID, remotePath, ok := parseMachinePath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}
	s.machinesMu.RLock()
	tunnel := s.machines[machineID]
	s.machinesMu.RUnlock()
	if tunnel == nil {
		http.Error(response, "machine is not connected to this relay", http.StatusBadGateway)
		return
	}
	stream, err := tunnel.open()
	if err != nil {
		http.Error(response, "open machine tunnel: "+err.Error(), http.StatusBadGateway)
		return
	}
	stream.setIdle(s.idle)
	defer stream.Close()
	s.logger.Printf("event=relay_pipe_open machine_id=%q method=%q path=%q", machineID, request.Method, remotePath)
	if remotePath == "/ws" && websocketUpgrade(request) {
		s.serveUpgraded(stream, response, request, remotePath)
		return
	}
	s.serveHTTP(stream, response, request, remotePath)
}

func (s *Server) serveHTTP(stream *stream, response http.ResponseWriter, request *http.Request, remotePath string) {
	forwarded := cloneRequest(request, remotePath)
	forwarded.Close = true
	removeHopHeaders(forwarded.Header)
	forwarded.Header.Set("Connection", "close")
	if err := forwarded.Write(stream); err != nil {
		http.Error(response, "write machine request: "+err.Error(), http.StatusBadGateway)
		return
	}
	machineResponse, err := http.ReadResponse(bufio.NewReader(stream), forwarded)
	if err != nil {
		http.Error(response, "read machine response: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer machineResponse.Body.Close()
	copyResponseHeaders(response.Header(), machineResponse.Header)
	response.WriteHeader(machineResponse.StatusCode)
	_, _ = io.Copy(response, machineResponse.Body)
}

func (s *Server) serveUpgraded(stream *stream, response http.ResponseWriter, request *http.Request, remotePath string) {
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "websocket relay requires HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	forwarded := cloneRequest(request, remotePath)
	if err := forwarded.Write(stream); err != nil {
		return
	}
	if buffered.Reader.Buffered() > 0 {
		_, _ = io.CopyN(stream, buffered.Reader, int64(buffered.Reader.Buffered()))
	}
	done := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(stream, client); _ = stream.CloseWrite(); done <- struct{}{} }()
	_, _ = io.Copy(client, stream)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func cloneRequest(request *http.Request, remotePath string) *http.Request {
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = "", ""
	clone.URL.Path, clone.URL.RawPath = remotePath, ""
	clone.RequestURI = ""
	clone.Host = "127.0.0.1"
	clone.Header = request.Header.Clone()
	stripProxyIdentity(clone.Header)
	clone.Header.Set("X-Forwarded-For", remoteIP(request.RemoteAddr))
	clone.Header.Set("X-Sessions-Relay-Forwarded", "1")
	return clone
}

func stripProxyIdentity(headers http.Header) {
	for _, key := range []string{
		"Forwarded", "Proxy-Authorization", "Tailscale-User-Login",
		"Tailscale-User-Name", "Tailscale-User-Profile-Pic", "X-Forwarded-For",
		"X-Forwarded-Host", "X-Forwarded-Proto", "X-Sessions-Relay-Forwarded",
	} {
		headers.Del(key)
	}
}

func parseMachinePath(path string) (string, string, bool) {
	remainder := strings.TrimPrefix(path, "/m/")
	parts := strings.SplitN(remainder, "/", 2)
	if len(parts) != 2 || !validMachineID(parts[0]) {
		return "", "", false
	}
	remote := "/" + parts[1]
	return parts[0], remote, strings.HasPrefix(remote, "/api/") || remote == "/ws"
}

func validMachineID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." || strings.Contains(value, "..") {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.", character)) {
			return false
		}
	}
	return true
}

func writeJSONMessage(ctx context.Context, connection *websocket.Conn, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, encoded)
}

func readJSONMessage(ctx context.Context, connection *websocket.Conn, value any) error {
	messageType, encoded, err := connection.Read(ctx)
	if err != nil {
		return err
	}
	if messageType != websocket.MessageText || len(encoded) > 4096 {
		return errors.New("invalid relay authentication message")
	}
	return json.Unmarshal(encoded, value)
}

func copyResponseHeaders(target, source http.Header) {
	source = source.Clone()
	removeHopHeaders(source)
	for key, values := range source {
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func removeHopHeaders(headers http.Header) {
	for _, name := range strings.Split(headers.Get("Connection"), ",") {
		headers.Del(strings.TrimSpace(name))
	}
	for _, key := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		headers.Del(key)
	}
}

func websocketUpgrade(request *http.Request) bool {
	return strings.EqualFold(request.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(request.Header.Get("Connection")), "upgrade")
}

func remoteIP(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		return host
	}
	return address
}

func closeReason(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return "closed"
	}
	return fmt.Sprintf("%v", err)
}
