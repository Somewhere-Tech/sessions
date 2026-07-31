package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/api"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

var anyHosts = map[string]struct{}{"0.0.0.0": {}, "::": {}, "::0": {}, "*": {}}
var version = "0.2.15"

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
	if _, refused := anyHosts[config.Host]; refused {
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
