package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

type ConnectorOptions struct {
	URL          func(context.Context) (string, error)
	Target       string
	Authenticate func(relayauth.Challenge) (relayauth.Response, error)
	HTTPClient   *http.Client
	Logger       *log.Logger
	BackoffMax   time.Duration
	OnConnection func(bool)
}

type Connector struct {
	options ConnectorOptions
	mu      sync.Mutex
	current *websocket.Conn
}

func NewConnector(options ConnectorOptions) *Connector { return &Connector{options: options} }

func (c *Connector) Run(ctx context.Context) {
	backoff := time.Second
	maximum := c.options.BackoffMax
	if maximum <= 0 {
		maximum = 30 * time.Second
	}
	for ctx.Err() == nil {
		err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		c.logger().Printf("event=relay_tunnel_retry delay=%q error=%q", backoff, err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maximum)
	}
}

func (c *Connector) RunOnce(ctx context.Context) error {
	if c.options.URL == nil || c.options.Authenticate == nil || strings.TrimSpace(c.options.Target) == "" {
		return errors.New("relay connector is incomplete")
	}
	base, err := c.options.URL(ctx)
	if err != nil {
		return err
	}
	endpoint, err := relayConnectURL(base)
	if err != nil {
		return err
	}
	if endpoint == "" {
		return errors.New("relay is not configured")
	}
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: c.options.HTTPClient})
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect relay: HTTP %d: %w", response.StatusCode, err)
		}
		return fmt.Errorf("connect relay: %w", err)
	}
	c.mu.Lock()
	c.current = connection
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.current == connection {
			c.current = nil
		}
		c.mu.Unlock()
	}()
	defer connection.Close(websocket.StatusNormalClosure, "daemon stopping")
	connection.SetReadLimit(MaxPayload + 1024)
	var challenge relayauth.Challenge
	if err := readJSONMessage(ctx, connection, &challenge); err != nil {
		return fmt.Errorf("read relay challenge: %w", err)
	}
	authentication, err := c.options.Authenticate(challenge)
	if err != nil {
		return fmt.Errorf("sign relay challenge: %w", err)
	}
	if err := writeJSONMessage(ctx, connection, authentication); err != nil {
		return fmt.Errorf("send relay authentication: %w", err)
	}
	var accepted struct {
		OK bool `json:"ok"`
	}
	if err := readJSONMessage(ctx, connection, &accepted); err != nil || !accepted.OK {
		return errors.New("relay rejected machine authentication")
	}
	if c.options.OnConnection != nil {
		c.options.OnConnection(true)
		defer c.options.OnConnection(false)
	}
	c.logger().Printf("event=relay_tunnel_connected machine_id=%q endpoint=%q", authentication.MachineID, base)
	tunnel := newTunnel(ctx, connection)
	return tunnel.readLoop(c.serveStream)
}

func (c *Connector) Disconnect() {
	c.mu.Lock()
	connection := c.current
	c.mu.Unlock()
	if connection != nil {
		_ = connection.Close(websocket.StatusNormalClosure, "relay setting changed")
	}
}

func (c *Connector) serveStream(stream *stream) {
	connection, err := net.DialTimeout("tcp", c.options.Target, 5*time.Second)
	if err != nil {
		_ = stream.Close()
		return
	}
	defer connection.Close()
	defer stream.Close()
	reader := bufio.NewReader(stream)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	request.Host = c.options.Target
	request.RequestURI = ""
	request.URL.Scheme, request.URL.Host = "", ""
	secureForwardedRequest(request)
	if err := request.Write(connection); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(stream, connection)
		_ = stream.CloseWrite()
		done <- struct{}{}
	}()
	_, _ = io.Copy(connection, reader)
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// The daemon accepts this connection from its own loopback listener. Enforce
// the remote-auth boundary here, rather than trusting the relay to supply it:
// even a compromised relay must still present a daemon-issued device token.
func secureForwardedRequest(request *http.Request) {
	stripProxyIdentity(request.Header)
	request.Header.Set("X-Forwarded-For", "sessions-relay")
	request.Header.Set("X-Sessions-Relay-Forwarded", "1")
}

func relayConnectURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("relay URL must be an HTTP(S) origin")
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", errors.New("relay URL must use HTTPS (or HTTP behind a local TLS proxy)")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/connect"
	return parsed.String(), nil
}

func MachineEndpoint(raw, machineID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return "", errors.New("relay URL must be an HTTP(S) origin")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/m/" + url.PathEscape(machineID)
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String(), nil
}

func BaseURL(raw, machineID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("relay URL must be an HTTP(S) origin")
	}
	suffix := "/m/" + url.PathEscape(machineID)
	parsed.Path = strings.TrimSuffix(parsed.Path, suffix)
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func (c *Connector) logger() *log.Logger {
	if c.options.Logger != nil {
		return c.options.Logger
	}
	return log.Default()
}
