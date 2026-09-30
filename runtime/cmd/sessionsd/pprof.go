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

// defaultPprofAddress is a loopback listener on a port the operating system
// chooses. It is on by default because the alternative — asking somebody to
// restart the daemon with an environment variable after the burst they wanted
// to profile has passed — is not an answer. What it exposes is the Go runtime's
// own stack traces and counters, to loopback only: no session content, no
// conversation, no credential, and nothing reachable from another machine.
const defaultPprofAddress = "127.0.0.1:0"

func startPprof(raw string) (*pprofListener, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "0") || strings.EqualFold(raw, "false") {
		return nil, nil
	}
	if raw == "" {
		raw = defaultPprofAddress
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
