package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const defaultRelayLabel = "tech.somewhere.sessions.relay"

type relayInstallOptions struct {
	Listen, Cert, Key, AllowFile, DirectoryURL, OwnerTokenFile string
}

func (a *app) cmdRelay(args []string) error {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return fail(1, "usage: sessions relay status")
		}
		return a.relayStatus()
	}
	if args[0] == "set" && len(args) == 2 {
		return a.setRelay(args[1])
	}
	if args[0] == "disable" && len(args) == 1 {
		return a.setRelay("")
	}
	if args[0] != "install" {
		return fail(1, "usage: sessions relay <status|set URL|disable|install [options]>")
	}
	args = args[1:]
	options, err := parseRelayInstallOptions(&args)
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions relay install [relay options]")
	}
	return a.installRelay(options)
}

type relayCLIState struct {
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
	Source    string `json:"source,omitempty"`
}

func (a *app) relayStatus() error {
	var state relayCLIState
	if err := a.getJSON("/api/relay", &state); err != nil {
		return err
	}
	return a.writeRelayState(state)
}

func (a *app) setRelay(url string) error {
	var state relayCLIState
	if err := a.putJSON("/api/relay", map[string]string{"url": url}, &state, 2); err != nil {
		return err
	}
	return a.writeRelayState(state)
}

func (a *app) writeRelayState(state relayCLIState) error {
	if a.wantJSON {
		return writeJSON(a.stdout, state, true)
	}
	if state.URL == "" {
		_, err := fmt.Fprintln(a.stdout, "Relay fallback is disabled.")
		return err
	}
	status := "connecting"
	if state.Connected {
		status = "connected"
	}
	_, err := fmt.Fprintf(a.stdout, "Relay fallback: %s (%s, source %s)\n", state.URL, status, state.Source)
	return err
}

func parseRelayInstallOptions(args *[]string) (relayInstallOptions, error) {
	options := relayInstallOptions{Listen: "127.0.0.1:8899"}
	fields := []struct {
		flag  string
		value *string
	}{
		{"--listen", &options.Listen}, {"--cert", &options.Cert}, {"--key", &options.Key},
		{"--allow-file", &options.AllowFile}, {"--directory-url", &options.DirectoryURL},
		{"--owner-token-file", &options.OwnerTokenFile},
	}
	for _, field := range fields {
		if value, found := pluck(args, field.flag); found {
			*field.value = value
		}
	}
	if (options.Cert == "") != (options.Key == "") {
		return options, fail(1, "--cert and --key must be supplied together")
	}
	if options.AllowFile == "" && options.DirectoryURL == "" {
		return options, fail(1, "relay install needs --allow-file or --directory-url with --owner-token-file")
	}
	if options.DirectoryURL != "" && options.OwnerTokenFile == "" {
		return options, fail(1, "--directory-url needs --owner-token-file")
	}
	return options, nil
}

func (a *app) installRelay(options relayInstallOptions) error {
	if runtime.GOOS != "darwin" {
		return fail(2, "relay install requires macOS launchd; use the Docker example in docs/RELAY.md on other hosts")
	}
	executable, _ := os.Executable()
	binary := locateInstallBinary("sessions-relay", os.Getenv("SESSIONS_RELAY_BINARY"), filepath.Dir(executable))
	relayMode := false
	if binary == "" {
		binary = locateInstallBinary("sessionsd", os.Getenv("SESSIONS_BINARY"), filepath.Dir(executable))
		relayMode = binary != ""
	}
	if binary == "" {
		return fail(2, "install is incomplete: neither sessions-relay nor sessionsd is beside sessions or on PATH")
	}
	launchctl := launchctlExecutable()
	if launchctl == "" {
		return fail(2, "relay install requires launchctl")
	}
	plistPath := filepath.Join(a.home, "Library", "LaunchAgents", defaultRelayLabel+".plist")
	logFile := filepath.Join(a.home, "Library", "Logs", "sessions", defaultRelayLabel+".log")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return err
	}
	arguments := relayInstallArguments(binary, relayMode, options)
	xml := daemonPlist(daemonPlistOptions{
		Label: defaultRelayLabel, ProgramArguments: arguments,
		WorkingDir: filepath.Dir(binary), LogFile: logFile,
	})
	if err := writeDaemonPlist(plistPath, xml); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	bootout := exec.Command(launchctl, "bootout", domain+"/"+defaultRelayLabel)
	output, bootoutErr := bootout.CombinedOutput()
	if bootoutErr != nil && !launchctlServiceMissing(output, bootoutErr) {
		return fail(2, "launchctl bootout before relay reinstall failed: %s", outputOrError(output, bootoutErr))
	}
	command := exec.Command(launchctl, "bootstrap", domain, plistPath)
	if output, err := command.CombinedOutput(); err != nil {
		return fail(2, "launchctl bootstrap relay failed: %s", outputOrError(output, err))
	}
	fmt.Fprintf(a.stdout, "sessions-relay installed and started.\n  Label: %s\n  Plist: %s\n  Logs:  %s\n", defaultRelayLabel, plistPath, logFile)
	return nil
}

func relayInstallArguments(binary string, relayMode bool, options relayInstallOptions) []string {
	arguments := []string{binary, "--listen", options.Listen}
	if relayMode {
		arguments = []string{binary, "--relay", "--listen", options.Listen}
	}
	for _, pair := range [][2]string{
		{"--cert", options.Cert}, {"--key", options.Key}, {"--allow-file", options.AllowFile},
		{"--directory-url", options.DirectoryURL}, {"--owner-token-file", options.OwnerTokenFile},
	} {
		if pair[1] != "" {
			arguments = append(arguments, pair[0], pair[1])
		}
	}
	return arguments
}
