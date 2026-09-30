package relay

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Each stream carries one parsed request, not a trusted loopback byte pipe.
// Opaque bidirectional bytes are permitted only after the daemon confirms the
// requested WebSocket upgrade. The parsed Body never includes a pipelined
// request; chunk framing and trailers are handled by net/http.
func forwardConnectorResponse(output io.Writer, reader *bufio.Reader, request *http.Request, wantsUpgrade bool) (bool, error) {
	for range 8 {
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return false, err
		}
		if response.StatusCode == http.StatusSwitchingProtocols {
			if !wantsUpgrade || !headerHasToken(response.Header, "Connection", "upgrade") ||
				!strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
				_ = response.Body.Close()
				return false, errors.New("daemon did not confirm the requested WebSocket upgrade")
			}
			// Never let Response.Write consume opaque protocol bytes as an HTTP
			// body; those remain in reader for the subsequent duplex copy.
			response.Body = http.NoBody
			return true, response.Write(output)
		}
		err = response.Write(output)
		_ = response.Body.Close()
		if err != nil {
			return false, err
		}
		if response.StatusCode >= http.StatusOK {
			return false, nil
		}
	}
	return false, errors.New("daemon sent too many informational responses")
}

func headerHasToken(headers http.Header, name, token string) bool {
	for _, value := range headers.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}
