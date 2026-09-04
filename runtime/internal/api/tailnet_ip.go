package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tailscale"
)

type tailnetIPRequestContextKey struct{}

type tailnetIPListener struct {
	mu       sync.RWMutex
	config   state.Config
	handler  http.Handler
	server   *http.Server
	listener net.Listener
	host     string
}

func newTailnetIPListener(config state.Config, handler http.Handler) *tailnetIPListener {
	return &tailnetIPListener{config: config, handler: handler}
}

func (l *tailnetIPListener) activeHost() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.host
}

func (l *tailnetIPListener) setEndpoint(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return l.close()
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Port() != strconv.Itoa(l.config.Port) {
		return errors.New("invalid tailnet-IP listener endpoint")
	}
	host := tailscale.TailnetIPv4([]string{parsed.Hostname()})
	if host == "" {
		return errors.New("tailnet-IP listener is outside 100.64.0.0/10")
	}
	l.mu.RLock()
	unchanged := l.server != nil && l.host == host
	l.mu.RUnlock()
	if unchanged {
		return nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(l.config.Port)))
	if err != nil {
		return fmt.Errorf("listen on %s only: %w", host, err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			ctx := context.WithValue(request.Context(), tailnetIPRequestContextKey{}, true)
			l.handler.ServeHTTP(response, request.WithContext(ctx))
		}),
		ReadHeaderTimeout: 65 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	l.mu.Lock()
	previousServer, previousListener := l.server, l.listener
	l.server, l.listener, l.host = server, listener, host
	l.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
	if previousServer != nil {
		_ = previousServer.Close()
	}
	if previousListener != nil {
		_ = previousListener.Close()
	}
	return nil
}

func (l *tailnetIPListener) close() error {
	l.mu.Lock()
	server, listener := l.server, l.listener
	l.server, l.listener, l.host = nil, nil, ""
	l.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

func isTailnetIPRequest(request *http.Request) bool {
	value, _ := request.Context().Value(tailnetIPRequestContextKey{}).(bool)
	return value
}

func (s *Server) CloseTailnetIP() error {
	return s.tailnetIP.close()
}
