//go:build darwin

package discovery

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dnsSDPath = "/usr/bin/dns-sd"

type dnsSDRegistration struct {
	command *exec.Cmd
	done    <-chan error
	once    sync.Once
	err     error
}

func platformAdvertise(address net.IP, port int, instance, hostLabel string) (Registration, error) {
	arguments := dnsSDProxyArguments(address, port, instance, hostLabel)
	command := exec.Command(dnsSDPath, arguments...)
	output, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare macOS Bonjour registration: %w", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start macOS Bonjour registration: %w", err)
	}

	active := make(chan struct{})
	var activeOnce sync.Once
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Got a reply for service") &&
				strings.Contains(line, "registered and active") {
				activeOnce.Do(func() { close(active) })
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	registration := &dnsSDRegistration{command: command, done: done}

	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	select {
	case <-active:
		return registration, nil
	case waitErr := <-done:
		if waitErr == nil {
			waitErr = errors.New("dns-sd exited before registration became active")
		}
		return nil, fmt.Errorf("macOS Bonjour registration failed: %w", waitErr)
	case <-timer.C:
		_ = registration.Shutdown()
		return nil, errors.New("macOS Bonjour registration did not become active within 4s")
	}
}

func dnsSDProxyArguments(address net.IP, port int, instance, hostLabel string) []string {
	return []string{
		"-P",
		instance,
		ServiceType,
		Domain,
		strconv.Itoa(port),
		hostLabel + ".local.",
		address.String(),
		"sessions=1",
		"api=1",
		"approval=required",
		"transport=http",
	}
}

func (registration *dnsSDRegistration) Shutdown() error {
	registration.once.Do(func() {
		if registration.command == nil || registration.command.Process == nil {
			return
		}
		signalErr := registration.command.Process.Signal(os.Interrupt)
		if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			registration.err = signalErr
		}
		select {
		case waitErr := <-registration.done:
			if waitErr != nil && registration.err == nil {
				var exitError *exec.ExitError
				if !errors.As(waitErr, &exitError) {
					registration.err = waitErr
				}
			}
		case <-time.After(2 * time.Second):
			if killErr := registration.command.Process.Kill(); killErr != nil &&
				!errors.Is(killErr, os.ErrProcessDone) && registration.err == nil {
				registration.err = killErr
			}
			<-registration.done
		}
	})
	return registration.err
}
