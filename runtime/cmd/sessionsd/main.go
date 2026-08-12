package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

var version = "0.2.21"

// isWildcardHost reports whether a bind host would expose the daemon on every
// interface. A literal denylist is not enough, and neither is netip.ParseAddr
// alone: net.Listen resolves the host through the system resolver, which is
// more permissive than the parser. "0.0.0.000" is rejected by netip but still
// binds every interface, so the final check asks the resolver the same question
// the listener will ask.
func isWildcardHost(host string) bool {
	trimmed := strings.Trim(strings.TrimSpace(host), "[]")
	if trimmed == "" || trimmed == "*" {
		return true
	}
	if address, err := netip.ParseAddr(trimmed); err == nil {
		return address.IsUnspecified()
	}
	// An unresolvable host is not a wildcard; net.Listen will fail on its own
	// with a clearer message than this guard could produce.
	resolved, err := net.LookupIP(trimmed)
	if err != nil {
		return false
	}
	for _, ip := range resolved {
		if ip.IsUnspecified() {
			return true
		}
	}
	return false
}

func main() {
	handled, err := runPlatformSupervisor(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if handled {
		return
	}
	config, err := state.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if isWildcardHost(config.Host) {
		fmt.Fprintf(os.Stderr,
			"\n  sessionsd: refusing to bind to %s.\n  Set SESSIONS_HOST to a specific address — 127.0.0.1 for loopback only,\n  or a tailnet IP (100.x.y.z) for access from other devices on your tailnet.\n\n",
			config.Host,
		)
		os.Exit(2)
	}
	if os.Getenv("SESSIONS_SMOKE") == "1" {
		return
	}

	ledgerStore, err := ledger.Open(context.Background(), ledger.Options{})
	if err != nil {
		log.Fatalf("open lane ledger: %v", err)
	}
	defer func() {
		if err := ledgerStore.Close(); err != nil {
			log.Printf("close lane ledger: %v", err)
		}
	}()
	usageService := usage.NewLocalService(config)
	defer func() {
		if err := usageService.Close(); err != nil {
			log.Printf("close usage ledger: %v", err)
		}
	}()
	manager := session.NewManager(config, state.NewPlatformLauncher(config), session.ManagerOptions{
		Boundaries: ledgerStore.Boundaries(), Observations: ledgerStore.Observations(), LedgerReader: ledgerStore,
		Retention:     ledgerStore.Retention(),
		Attributions:  ledgerStore.Attributions(),
		UsageRecorder: usageService,
	})
	defer manager.Close()
	api.Version = version
	handler := api.NewWithUsage(config, manager, usageService, manager.Push())
	// An explicitly isolated scratch daemon must not restore the user's
	// persisted LAN listener on a second port.
	if os.Getenv("SESSIONS_STATE_DIR") == "" {
		handler.RestoreLAN(log.Printf)
	}
	defer func() {
		if err := handler.CloseLAN(); err != nil {
			log.Printf("close LAN listener: %v", err)
		}
	}()
	server := &http.Server{
		Addr: config.ListenAddress(), Handler: handler,
		ReadHeaderTimeout: 65 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Sweep before discovery so the first pass sees a runner directory and a
	// LaunchAgents directory that describe only sessions that might still be
	// running. The sweep removes nothing for a session that is live, unknown,
	// or starting; see Manager.SweepStaleRunnerArtifacts.
	manager.SweepStaleRunnerArtifacts(context.Background())
	go manager.RunDiscoveryLoop()
	serveErrors := make(chan error, 1)
	go func() {
		log.Printf("sessionsd listening on http://%s", config.ListenAddress())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrors <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	cleanupPlatformStop := watchPlatformStop(stop)
	defer cleanupPlatformStop()
	select {
	case sig := <-stop:
		log.Printf("sessionsd: %s received, shutting down", sig)
	case err := <-serveErrors:
		log.Printf("sessionsd: server error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("sessionsd shutdown: %v", err)
	}
}
