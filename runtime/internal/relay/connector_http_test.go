package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectorStreamCancellationClosesStalledBackend(t *testing.T) {
	for _, scope := range []string{"stream", "tunnel"} {
		t.Run(scope, func(t *testing.T) {
			requested := make(chan struct{})
			cancelled := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				close(requested)
				<-request.Context().Done()
				close(cancelled)
			}))
			t.Cleanup(backend.Close)
			upstream := connectorFixtureStream(t, backend.URL)
			if _, err := io.WriteString(upstream, "GET /api/stalled HTTP/1.1\r\nHost: fixture.invalid\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requested:
			case <-time.After(2 * time.Second):
				t.Fatal("backend did not receive the parsed request")
			}
			if scope == "stream" {
				if err := upstream.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = upstream.tunnel.connection.CloseNow()
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not close the stalled backend connection")
			}
		})
	}
}

func TestConnectorPreservesContentLengthAndChunkedTrailersWithoutPipeline(t *testing.T) {
	for _, framing := range []string{
		"Content-Length: 7\r\n\r\npayload",
		"Transfer-Encoding: chunked\r\nTrailer: X-Fixture-Trailer\r\n\r\n7\r\npayload\r\n0\r\nX-Fixture-Trailer: present\r\n\r\n",
	} {
		t.Run(strings.Fields(framing)[0], func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != "payload" || request.Header.Get("X-Forwarded-For") != "sessions-relay" || !request.Close {
					t.Errorf("forwarded request body=%q error=%v marker=%q close=%t", body, err, request.Header.Get("X-Forwarded-For"), request.Close)
				}
				if strings.HasPrefix(framing, "Transfer-Encoding") && request.Trailer.Get("X-Fixture-Trailer") != "present" {
					t.Errorf("chunked trailer = %q", request.Trailer.Get("X-Fixture-Trailer"))
				}
				_, _ = io.WriteString(response, "accepted")
			}))
			defer backend.Close()
			upstream := connectorFixtureStream(t, backend.URL)
			_, err := io.WriteString(upstream, "POST /api/body HTTP/1.1\r\nHost: fixture.invalid\r\n"+framing)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(upstream), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "accepted" {
				t.Fatalf("response body = %q error=%v", body, err)
			}
		})
	}
}

func TestConnectorVerifiedUpgradePreservesImmediatelyBufferedDuplexBytes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, buffered, err := response.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nearly-backend-bytes")
		_ = buffered.Flush()
		payload := make([]byte, len("early-client-bytes"))
		if _, err := io.ReadFull(buffered.Reader, payload); err != nil || string(payload) != "early-client-bytes" {
			t.Errorf("post101 client bytes = %q, error=%v", payload, err)
		}
		_, _ = connection.Write([]byte("duplex-confirmed"))
	}))
	defer backend.Close()
	upstream := connectorFixtureStream(t, backend.URL)
	_, err := io.WriteString(upstream, "GET /ws HTTP/1.1\r\nHost: fixture.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nearly-client-bytes")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(upstream)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, error=%v", response, err)
	}
	expected := "early-backend-bytesduplex-confirmed"
	payload := make([]byte, len(expected))
	if _, err := io.ReadFull(reader, payload); err != nil || string(payload) != expected {
		t.Fatalf("post101 backend bytes = %q, error=%v", payload, err)
	}
}

func TestConnectorResponseLimitsInformationalAndConfirmsOnlyWebSocket101(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "http://fixture.invalid/ws", nil)
	for _, fixture := range []struct {
		name, body              string
		wants, upgraded, failed bool
	}{
		{"100 then200", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", false, false, false},
		{"too many100", strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", 9), false, false, true},
		{"unsolicited101", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", false, false, true},
		{"wrong101 protocol", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: other\r\n\r\n", true, false, true},
		{"wrong connection token", "HTTP/1.1 101 Switching Protocols\r\nConnection: notupgrade\r\nUpgrade: websocket\r\n\r\n", true, false, true},
		{"confirmed101", "HTTP/1.1 101 Switching Protocols\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n", true, true, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var output bytes.Buffer
			upgraded, err := forwardConnectorResponse(&output, bufio.NewReader(strings.NewReader(fixture.body)), request, fixture.wants)
			if upgraded != fixture.upgraded || (err != nil) != fixture.failed {
				t.Fatalf("upgraded=%t error=%v", upgraded, err)
			}
			if fixture.name == "100 then200" && !strings.Contains(output.String(), "100 Continue") ||
				fixture.name == "100 then200" && !strings.HasSuffix(output.String(), "ok") {
				t.Fatal(fmt.Sprintf("interim/final response not preserved: %q", output.String()))
			}
		})
	}
}
