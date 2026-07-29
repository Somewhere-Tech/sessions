package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	tailscaleDownloadURL = "https://tailscale.com/download"
	tailscaleDNSAdminURL = "https://login.tailscale.com/admin/dns"
)

type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
	Self         *struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

type serveHandler struct {
	Proxy string `json:"Proxy"`
}

type serveWeb struct {
	Handlers map[string]serveHandler `json:"Handlers"`
}

type serveJSON struct {
	Web map[string]serveWeb `json:"Web"`
}

type serveState struct {
	Endpoint  string
	RootProxy string
	JSON      *serveJSON
	Text      string
}

type commandResult struct {
	status int
	stdout string
	stderr string
	err    error
}

func runTailscale(args ...string) commandResult {
	command := exec.Command("tailscale", args...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		status = -1
		if exitError, ok := err.(*exec.ExitError); ok {
			status = exitError.ExitCode()
		}
	}
	return commandResult{status: status, stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func commandResultText(result commandResult) string {
	parts := make([]string, 0, 2)
	if result.stdout != "" {
		parts = append(parts, result.stdout)
	}
	if result.stderr != "" {
		parts = append(parts, result.stderr)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func preflightTailscale() (tailscaleStatus, error) {
	versionResult := runTailscale("version")
	if versionResult.err != nil {
		var executableError *exec.Error
		if errors.As(versionResult.err, &executableError) || errors.Is(versionResult.err, exec.ErrNotFound) {
			return tailscaleStatus{}, fail(1, "Tailscale is not installed. Download it: %s", tailscaleDownloadURL)
		}
	}
	if versionResult.status != 0 {
		detail := commandResultText(versionResult)
		if detail == "" && versionResult.err != nil {
			detail = versionResult.err.Error()
		}
		if detail == "" {
			detail = "unknown error"
		}
		return tailscaleStatus{}, fail(2, "could not run `tailscale version`: %s", detail)
	}
	statusResult := runTailscale("status", "--json")
	var status tailscaleStatus
	if statusResult.status == 0 {
		_ = json.Unmarshal([]byte(statusResult.stdout), &status)
	}
	if status.BackendState != "Running" || status.Self == nil {
		return tailscaleStatus{}, fail(1, "Tailscale is not connected. Run: tailscale up")
	}
	return status, nil
}

func normalizeProxyTarget(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" {
		return strings.TrimSuffix(target, "/")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return strings.TrimSuffix(parsed.String(), "/")
}

func endpointFromAuthority(authority string) string {
	parsed, err := url.Parse("https://" + authority)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return "https://" + parsed.Host
}

func endpointFromServeJSON(status *serveJSON, target string) string {
	if status == nil || status.Web == nil {
		return ""
	}
	wanted := ""
	if target != "" {
		wanted = normalizeProxyTarget(target)
	}
	first := ""
	for authority, web := range status.Web {
		endpoint := endpointFromAuthority(authority)
		if first == "" && endpoint != "" {
			first = endpoint
		}
		if wanted == "" {
			continue
		}
		for _, handler := range web.Handlers {
			if handler.Proxy != "" && normalizeProxyTarget(handler.Proxy) == wanted {
				return endpoint
			}
		}
	}
	if wanted != "" {
		return ""
	}
	return first
}

func rootHandlerFromServeJSON(status *serveJSON) (string, string) {
	if status == nil {
		return "", ""
	}
	for authority, web := range status.Web {
		if handler, ok := web.Handlers["/"]; ok {
			return endpointFromAuthority(authority), handler.Proxy
		}
	}
	return "", ""
}

func endpointFromServeText(text string) string {
	for _, field := range strings.Fields(text) {
		field = strings.Trim(field, "()")
		if !strings.HasPrefix(strings.ToLower(field), "https://") {
			continue
		}
		if parsed, err := url.Parse(field); err == nil && parsed.Host != "" {
			return parsed.Scheme + "://" + parsed.Host
		}
	}
	return ""
}

func readServeStatus(target string) (serveState, error) {
	jsonResult := runTailscale("serve", "status", "--json")
	if jsonResult.status == 0 {
		var parsed serveJSON
		if json.Unmarshal([]byte(jsonResult.stdout), &parsed) == nil {
			_, rootProxy := rootHandlerFromServeJSON(&parsed)
			return serveState{
				Endpoint: endpointFromServeJSON(&parsed, target), RootProxy: rootProxy,
				JSON: &parsed, Text: strings.TrimSpace(jsonResult.stdout),
			}, nil
		}
	}
	textResult := runTailscale("serve", "status")
	if textResult.status != 0 {
		detail := commandResultText(textResult)
		if detail == "" {
			detail = "unknown error"
		}
		return serveState{}, fail(2, "could not read Tailscale Serve status: %s", detail)
	}
	return serveState{
		Endpoint: endpointFromServeText(textResult.stdout), Text: strings.TrimSpace(textResult.stdout),
	}, nil
}

func formatDaemonTarget(host string, port any) (string, error) {
	if strings.TrimSpace(host) == "" {
		return "", errors.New("daemon health returned no listen host")
	}
	parsedPort, err := strconv.Atoi(fmt.Sprint(port))
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return "", fmt.Errorf("daemon health returned invalid listen port: %v", port)
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return "http://" + net.JoinHostPort(host, strconv.Itoa(parsedPort)), nil
}

func isMagicDNSResolutionError(err error) bool {
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsNotFound {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such host") ||
		strings.Contains(message, "name or service not known") ||
		strings.Contains(message, "nodename nor servname provided")
}

func verifyEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	lookupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupIPAddr(lookupCtx, parsed.Hostname()); err != nil {
		return err
	}
	healthURL, err := url.Parse(endpoint + "/api/health")
	if err != nil {
		return err
	}
	request, _ := http.NewRequestWithContext(lookupCtx, http.MethodGet, healthURL.String(), nil)
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("%s returned HTTP %d", healthURL.String(), response.StatusCode)
	}
	return nil
}

func printQR(output io.Writer, connectURL string) error {
	code, err := qrcode.New(connectURL, qrcode.Medium)
	if err != nil {
		return err
	}
	io.WriteString(output, "\nScan on your phone:\n")
	bitmap := code.Bitmap()
	for row := 0; row < len(bitmap); row += 2 {
		for column := 0; column < len(bitmap[row]); column++ {
			top := bitmap[row][column]
			bottom := row+1 < len(bitmap) && bitmap[row+1][column]
			switch {
			case top && bottom:
				io.WriteString(output, "█")
			case top:
				io.WriteString(output, "▀")
			case bottom:
				io.WriteString(output, "▄")
			default:
				io.WriteString(output, " ")
			}
		}
		io.WriteString(output, "\n")
	}
	return nil
}

func (a *app) printConnection(endpoint string, target *string) error {
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Enabled    bool    `json:"enabled"`
			Verified   bool    `json:"verified"`
			Endpoint   string  `json:"endpoint"`
			Target     *string `json:"target"`
			ConnectURL string  `json:"connectUrl"`
		}{true, true, endpoint, target, endpoint}, false)
	}
	io.WriteString(a.stdout, "\nRemote access verified (HTTP 200).\n")
	fmt.Fprintf(a.stdout, "  Endpoint: %s\n", endpoint)
	_, err := io.WriteString(a.stdout, "Sessions apps on this tailnet can now discover this Mac and request access.\n")
	return err
}

func failRemoteVerification(err error, endpoint string) error {
	if isMagicDNSResolutionError(err) {
		return fail(2, "Tailscale Serve is configured at %s, but that .ts.net name does not resolve on this machine.\nEnable Tailscale DNS locally with `tailscale set --accept-dns=true`, and make sure MagicDNS is enabled at\n%s. Then retry: sessions remote status\nRemote access was not verified; not reporting success.", endpoint, tailscaleDNSAdminURL)
	}
	return fail(2, "Tailscale Serve is configured at %s, but HTTPS verification failed: %s\nRemote access was not verified.", endpoint, err)
}

func (a *app) cmdRemote(args []string) error {
	if len(args) != 1 {
		return fail(1, "usage: sessions remote <enable|disable|status>")
	}
	switch args[0] {
	case "enable":
		return a.remoteEnable()
	case "status":
		return a.remoteStatus()
	case "disable":
		return a.remoteDisable()
	default:
		return fail(1, "usage: sessions remote <enable|disable|status>")
	}
}

func (a *app) remoteEnable() error {
	status, err := preflightTailscale()
	if err != nil {
		return err
	}
	listenHost, listenPort, err := a.daemonListen(status)
	if err != nil {
		return err
	}
	target, err := formatDaemonTarget(listenHost, listenPort)
	if err != nil {
		return fail(2, "%s", err)
	}
	io.WriteString(a.stderr, "Privacy notice: enabling Tailscale HTTPS issues a public certificate.\nYour machine’s .ts.net name will be visible in public Certificate Transparency logs.\n\n")
	command := exec.Command("tailscale", "serve", "--bg", target)
	command.Stdin = nil
	command.Stdout = a.stderr
	command.Stderr = a.stderr
	if err := command.Run(); err != nil {
		return fail(2, "could not enable Tailscale Serve. If a one-time HTTPS approval URL appears above, open it and retry `sessions remote enable`.")
	}
	serve, err := readServeStatus(target)
	if err != nil {
		return err
	}
	if serve.Endpoint == "" {
		return fail(2, "Tailscale Serve did not report an HTTPS endpoint proxying %s; remote access was not verified.", target)
	}
	if err := verifyEndpoint(serve.Endpoint); err != nil {
		return failRemoteVerification(err, serve.Endpoint)
	}
	return a.printConnection(serve.Endpoint, &target)
}

func (a *app) daemonListen(status tailscaleStatus) (string, int, error) {
	type candidate struct {
		scheme, host string
		port         int
	}
	port, _ := strconv.Atoi(a.port)
	resolved, err := a.api.target("/api/health")
	if err != nil {
		return "", 0, err
	}
	candidates := []candidate{{resolved.Scheme, resolved.Hostname(), parsedURLPort(resolved, port)}}
	for _, host := range status.Self.TailscaleIPs {
		duplicate := false
		for _, existing := range candidates {
			if existing.host == host && existing.port == port {
				duplicate = true
			}
		}
		if !duplicate {
			candidates = append(candidates, candidate{"http", host, port})
		}
	}
	for _, candidate := range candidates {
		target := (&url.URL{Scheme: candidate.scheme, Host: net.JoinHostPort(candidate.host, strconv.Itoa(candidate.port)), Path: "/api/health"}).String()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			cancel()
			continue
		}
		var health struct {
			OK     bool   `json:"ok"`
			Name   string `json:"name"`
			Listen struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			} `json:"listen"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		cancel()
		if response.StatusCode != 200 || decodeErr != nil || !health.OK || health.Name != "sessionsd" {
			continue
		}
		if health.Listen.Host != "" {
			return health.Listen.Host, health.Listen.Port, nil
		}
		return candidate.host, candidate.port, nil
	}
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, net.JoinHostPort(candidate.host, strconv.Itoa(candidate.port)))
	}
	return "", 0, fail(2, "cannot reach sessionsd at %s. Start it first with `sessions install`.", strings.Join(parts, ", "))
}

func parsedURLPort(parsed *url.URL, fallback int) int {
	if parsed.Port() == "" {
		return fallback
	}
	value, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return fallback
	}
	return value
}

func (a *app) remoteStatus() error {
	if _, err := preflightTailscale(); err != nil {
		return err
	}
	serve, err := readServeStatus("")
	if err != nil {
		return err
	}
	if serve.Endpoint == "" {
		if a.wantJSON {
			return writeJSON(a.stdout, struct {
				Enabled  bool `json:"enabled"`
				Verified bool `json:"verified"`
			}{false, false}, false)
		}
		_, err := io.WriteString(a.stdout, "Remote access is disabled (no Tailscale Serve HTTPS endpoint).\n")
		return err
	}
	if err := verifyEndpoint(serve.Endpoint); err != nil {
		// A machine may deliberately keep Tailscale DNS disabled while its
		// peers still resolve and reach the Serve hostname. Native clients
		// need the configured state so they can pair from those peers; retain
		// the stricter non-JSON CLI diagnostic below.
		if a.wantJSON && isMagicDNSResolutionError(err) {
			return writeJSON(a.stdout, struct {
				Enabled           bool   `json:"enabled"`
				Verified          bool   `json:"verified"`
				Endpoint          string `json:"endpoint"`
				ConnectURL        string `json:"connectUrl"`
				VerificationError string `json:"verificationError"`
			}{true, false, serve.Endpoint, serve.Endpoint, "This Mac does not use Tailscale DNS; verify discovery from another tailnet device."}, false)
		}
		return failRemoteVerification(err, serve.Endpoint)
	}
	return a.printConnection(serve.Endpoint, nil)
}

func (a *app) remoteDisable() error {
	if _, err := preflightTailscale(); err != nil {
		return err
	}
	before, err := readServeStatus("")
	if err != nil {
		return err
	}
	if before.Endpoint == "" || (before.JSON != nil && before.RootProxy == "") {
		if a.wantJSON {
			return writeJSON(a.stdout, struct {
				Enabled bool `json:"enabled"`
				Changed bool `json:"changed"`
			}{false, false}, false)
		}
		_, err := io.WriteString(a.stdout, "Remote access is already disabled.\n")
		return err
	}
	result := runTailscale("serve", "--https=443", "--set-path=/", "off")
	output := commandResultText(result)
	if output != "" {
		fmt.Fprintln(a.stderr, output)
	}
	if result.status != 0 {
		if output == "" {
			output = "unknown error"
		}
		return fail(2, "could not disable Tailscale Serve: %s", output)
	}
	after, err := readServeStatus("")
	if err != nil {
		return err
	}
	stillEnabled := after.Endpoint == before.Endpoint
	if after.JSON != nil {
		stillEnabled = after.RootProxy != ""
	}
	if stillEnabled {
		return fail(2, "Tailscale still reports %s; remote access was not disabled.", before.Endpoint)
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			Enabled bool `json:"enabled"`
			Changed bool `json:"changed"`
		}{false, true}, false)
	}
	_, err = io.WriteString(a.stdout, "Remote access disabled.\n")
	return err
}
