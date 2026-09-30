package relaycmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/relay"
)

type Config struct {
	Listen         string
	Cert           string
	Key            string
	AllowFile      string
	DirectoryURL   string
	OwnerTokenFile string
}

func Run(arguments []string, output, errorOutput io.Writer) error {
	_ = output
	config, help, err := parse(arguments, errorOutput)
	if err != nil || help {
		return err
	}
	authorizer, err := authorizer(config)
	if err != nil {
		return err
	}
	logger := log.New(errorOutput, "sessions-relay ", log.LstdFlags|log.LUTC)
	handler := relay.NewServer(relay.ServerOptions{Authorizer: authorizer, Logger: logger})
	server := &http.Server{
		Addr: config.Listen, Handler: handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	errorsChannel := make(chan error, 1)
	go func() {
		logger.Printf("event=relay_listening address=%q tls=%t", config.Listen, config.Cert != "")
		if config.Cert != "" {
			errorsChannel <- server.ListenAndServeTLS(config.Cert, config.Key)
		} else {
			errorsChannel <- server.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func parse(arguments []string, errorOutput io.Writer) (Config, bool, error) {
	set := flag.NewFlagSet("sessions-relay", flag.ContinueOnError)
	set.SetOutput(errorOutput)
	var config Config
	set.StringVar(&config.Listen, "listen", "127.0.0.1:8899", "listen address")
	set.StringVar(&config.Cert, "cert", "", "TLS certificate PEM")
	set.StringVar(&config.Key, "key", "", "TLS private key PEM")
	set.StringVar(&config.AllowFile, "allow-file", "", "machine public-key allow-list")
	set.StringVar(&config.DirectoryURL, "directory-url", "", "reserved directory origin (unsupported; use --allow-file)")
	set.StringVar(&config.OwnerTokenFile, "owner-token-file", "", "reserved owner token file (unsupported; use --allow-file)")
	set.Usage = func() {
		fmt.Fprintln(errorOutput, "Usage: sessions-relay [--listen :8899] [--cert cert.pem --key key.pem] --allow-file machines.json")
	}
	if err := set.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config, true, nil
		}
		return config, false, err
	}
	if len(set.Args()) != 0 {
		return config, false, fmt.Errorf("unexpected relay arguments: %s", strings.Join(set.Args(), " "))
	}
	if (config.Cert == "") != (config.Key == "") {
		return config, false, errors.New("--cert and --key must be supplied together")
	}
	return config, false, nil
}

func authorizer(config Config) (relay.Authorizer, error) {
	authorizers := relay.AnyAuthorizer{}
	if config.AllowFile != "" {
		authorizers = append(authorizers, relay.AllowListAuthorizer{Path: config.AllowFile})
	}
	if config.DirectoryURL != "" {
		return nil, relay.ErrDirectoryAuthorizationUnavailable
	}
	if config.OwnerTokenFile != "" {
		return nil, relay.ErrDirectoryAuthorizationUnavailable
	}
	if len(authorizers) == 0 {
		return nil, errors.New("configure --allow-file with the allowed machine public keys")
	}
	return authorizers, nil
}
