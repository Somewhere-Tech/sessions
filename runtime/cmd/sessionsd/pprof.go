package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type pprofListener struct {
	server   *http.Server
	listener net.Listener
}

func daemonConfig() (state.Config, *pprofListener) {
	config, err := state.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	profiler, err := startPprof(os.Getenv("SESSIONS_PPROF"))
	if err != nil {
		log.Fatal(err)
	}
	config.PprofAddress = profiler.address()
	return config, profiler
}

func startPprof(raw string) (*pprofListener, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid SESSIONS_PPROF %q: %w", raw, err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 0 || portNumber > 65535 {
		return nil, fmt.Errorf("SESSIONS_PPROF must be a loopback IP and port, for example 127.0.0.1:6060")
	}
	listener, err := net.Listen("tcp", raw)
	if err != nil {
		return nil, fmt.Errorf("listen for pprof on %s: %w", raw, err)
	}
	server := &http.Server{
		Handler: http.DefaultServeMux, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second,
	}
	result := &pprofListener{server: server, listener: listener}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("sessionsd pprof: %v", err)
		}
	}()
	return result, nil
}

func (p *pprofListener) address() string {
	if p == nil {
		return ""
	}
	return p.listener.Addr().String()
}

func (p *pprofListener) close() {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.server.Shutdown(ctx)
}
