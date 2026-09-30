package relay

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Exercise the production connector with an intentionally untrusted tunnel
// peer, not the production relay's additional forwarding safeguards.
func connectorFixtureStream(t *testing.T, backend string) *stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(response, request, nil)
		if err == nil {
			accepted <- connection
			<-ctx.Done()
		}
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConnection := <-accepted
	t.Cleanup(func() { _ = client.CloseNow(); _ = serverConnection.CloseNow(); cancel() })
	target, _ := url.Parse(backend)
	connector := NewConnector(ConnectorOptions{Target: target.Host})
	local := newTunnel(ctx, serverConnection)
	peer := newTunnel(ctx, client)
	go func() { _ = local.readLoop(connector.serveStream) }()
	go func() { _ = peer.readLoop(nil) }()
	opened, err := peer.open()
	if err != nil {
		t.Fatal(err)
	}
	opened.setIdle(3 * time.Second)
	return opened
}

func TestConnectorRefusesPipelinedLoopbackAuthorityAfterOrdinaryOrFailedUpgrade(t *testing.T) {
	for _, first := range []string{
		"GET /api/check HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: keep-alive\r\n\r\n",
		"GET /api/check HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: keep-alive, X-Forwarded-For, X-Sessions-Relay-Forwarded\r\n\r\n",
		"GET /ws HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
	} {
		t.Run(strings.Fields(first)[1], func(t *testing.T) {
			var localAuthority atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("X-Sessions-Relay-Forwarded") != "1" {
					t.Error("connector did not retain its remote forwarding marker")
				}
				if request.Header.Get("X-Forwarded-For") == "" {
					localAuthority.Add(1)
					_, _ = io.WriteString(response, "local-admin-granted")
					return
				}
				http.Error(response, "device credential required", http.StatusUnauthorized)
			}))
			defer backend.Close()
			upstream := connectorFixtureStream(t, backend.URL)
			pipeline := first + "GET /api/admin HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: close\r\n\r\n"
			if _, err := io.WriteString(upstream, pipeline); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(upstream)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("first request = %d, want401", response.StatusCode)
			}
			second, err := http.ReadResponse(reader, nil)
			if err == nil {
				body, _ := io.ReadAll(second.Body)
				_ = second.Body.Close()
				t.Fatalf("pipelined request reached loopback backend: status=%d body=%q", second.StatusCode, body)
			}
			if localAuthority.Load() != 0 {
				t.Fatalf("pipelined request received local authority %d times", localAuthority.Load())
			}
		})
	}
}
