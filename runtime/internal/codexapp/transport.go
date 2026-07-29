package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	proxyWebSocketURL = "ws://localhost/rpc"
	maxMessageBytes   = 128 << 20
	writeTimeout      = 10 * time.Second
)

type messageTransport interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}

// proxyWebSocketTransport performs the HTTP Upgrade and websocket framing
// over the byte-transparent `codex app-server proxy` pipes.
type proxyWebSocketTransport struct {
	conn *websocket.Conn
}

func newProxyWebSocketTransport(
	ctx context.Context,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
) (*proxyWebSocketTransport, error) {
	stream := &stdioConn{reader: stdout, writer: stdin}
	var dialMu sync.Mutex
	dialed := false
	httpTransport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialMu.Lock()
			defer dialMu.Unlock()
			if dialed {
				return nil, errors.New("app-server proxy stream already dialed")
			}
			dialed = true
			return stream, nil
		},
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(dialCtx, proxyWebSocketURL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: httpTransport},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		_ = stream.Close()
		return nil, fmt.Errorf("upgrade app-server proxy to websocket: %w", err)
	}
	conn.SetReadLimit(maxMessageBytes)
	return &proxyWebSocketTransport{conn: conn}, nil
}

func (t *proxyWebSocketTransport) Read(ctx context.Context) ([]byte, error) {
	messageType, data, err := t.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageText {
		return nil, fmt.Errorf("unexpected app-server websocket message type %d", messageType)
	}
	return data, nil
}

func (t *proxyWebSocketTransport) Write(ctx context.Context, data []byte) error {
	return t.conn.Write(ctx, websocket.MessageText, data)
}

func (t *proxyWebSocketTransport) Close() error {
	return t.conn.CloseNow()
}

type stdioConn struct {
	reader io.ReadCloser
	writer io.WriteCloser
	once   sync.Once
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *stdioConn) Close() error {
	var first error
	c.once.Do(func() {
		if err := c.writer.Close(); err != nil {
			first = err
		}
		if err := c.reader.Close(); err != nil && first == nil {
			first = err
		}
	})
	return first
}
func (c *stdioConn) LocalAddr() net.Addr              { return stdioAddr("proxy-stdin") }
func (c *stdioConn) RemoteAddr() net.Addr             { return stdioAddr("app-server-proxy") }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type stdioAddr string

func (a stdioAddr) Network() string { return "stdio" }
func (a stdioAddr) String() string  { return string(a) }

// jsonlTransport is the direct-stdio protocol adapter used by deterministic
// tests. Production proxy traffic always uses proxyWebSocketTransport.
type jsonlTransport struct {
	reader  io.ReadCloser
	writer  io.WriteCloser
	decoder *json.Decoder
}

func newJSONLTransport(writer io.WriteCloser, reader io.ReadCloser) *jsonlTransport {
	return &jsonlTransport{reader: reader, writer: writer, decoder: json.NewDecoder(reader)}
}

func (t *jsonlTransport) Read(context.Context) ([]byte, error) {
	var message json.RawMessage
	if err := t.decoder.Decode(&message); err != nil {
		return nil, err
	}
	return message, nil
}

func (t *jsonlTransport) Write(_ context.Context, data []byte) error {
	data = append(append([]byte(nil), data...), '\n')
	for len(data) > 0 {
		n, err := t.writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (t *jsonlTransport) Close() error {
	first := t.writer.Close()
	if err := t.reader.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
