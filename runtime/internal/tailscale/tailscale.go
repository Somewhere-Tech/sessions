package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

const macOSCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// Status is the low-sensitivity part of `tailscale status --json` needed by
// Sessions. Endpoint is derived from Self.DNSName; no peer list is retained.
type Status struct {
	Present      bool     `json:"present"`
	SignedIn     bool     `json:"signedIn"`
	Endpoint     string   `json:"endpoint,omitempty"`
	TailnetIPv4  string   `json:"tailnetIPv4,omitempty"`
	TailscaleIPs []string `json:"-"`
}

type wireStatus struct {
	BackendState string `json:"BackendState"`
	Self         *struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

type serveHandler struct {
	Proxy string `json:"Proxy"`
}

type serveWeb struct {
	Handlers map[string]serveHandler `json:"Handlers"`
}

type serveStatus struct {
	Web map[string]serveWeb `json:"Web"`
}

// ParseStatus validates the status shape rather than treating malformed CLI
// output as a signed-in tailnet. It is exported so the CLI and daemon share
// the exact same interpretation.
func ParseStatus(encoded []byte) (Status, error) {
	var wire wireStatus
	if err := json.Unmarshal(encoded, &wire); err != nil {
		// The CLI prints prose instead of JSON when it cannot reach its
		// daemon or GUI; that sentence is the diagnosis, not the JSON error.
		if text := strings.TrimSpace(string(encoded)); text != "" && !strings.HasPrefix(text, "{") {
			return Status{}, fmt.Errorf("Tailscale CLI: %s", firstLine(text))
		}
		return Status{}, fmt.Errorf("decode Tailscale status: %w", err)
	}
	result := Status{Present: true}
	if wire.BackendState != "Running" || wire.Self == nil {
		return result, nil
	}
	result.SignedIn = true
	result.Endpoint = endpointFromDNSName(wire.Self.DNSName)
	result.TailscaleIPs = append([]string(nil), wire.Self.TailscaleIPs...)
	result.TailnetIPv4 = TailnetIPv4(result.TailscaleIPs)
	return result, nil
}

// TailnetIPv4 accepts only Tailscale's CGNAT range. It must never turn an
// arbitrary interface reported by a malformed status response into a listener.
func TailnetIPv4(values []string) string {
	_, carrier, _ := net.ParseCIDR("100.64.0.0/10")
	for _, value := range values {
		ip := net.ParseIP(strings.TrimSpace(value)).To4()
		if ip != nil && carrier.Contains(ip) {
			return ip.String()
		}
	}
	return ""
}

func endpointFromDNSName(value string) string {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if host == "" || !strings.HasSuffix(host, ".ts.net") {
		return ""
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Hostname() != host {
		return ""
	}
	return parsed.String()
}

// FindCLI follows the native-app location first, then PATH. The app bundle is
// not normally on PATH even when Tailscale is installed and signed in.
func FindCLI() (string, error) {
	if info, err := os.Stat(macOSCLI); err == nil && !info.IsDir() {
		return macOSCLI, nil
	}
	path, err := exec.LookPath("tailscale")
	if err != nil {
		return "", err
	}
	return path, nil
}

type Client struct {
	Path string
}

func NewClient() (Client, error) {
	path, err := FindCLI()
	return Client{Path: path}, err
}

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, exec.ErrNotFound
	}
	command := exec.CommandContext(ctx, c.Path, args...)
	command.Stdin = nil
	command.Env = cliEnvironment(os.Environ())
	encoded, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(encoded))
		if detail == "" {
			detail = err.Error()
		}
		return nil, errors.New(detail)
	}
	return encoded, nil
}

// cliEnvironment is the environment the Tailscale CLI needs. The Mac App
// Store build decides whether it was run from a shell by looking for SHLVL;
// without it the CLI tries to start the GUI itself and fails under launchd
// ("The Tailscale GUI failed to start"), which is how sessionsd runs.
func cliEnvironment(base []string) []string {
	env := append([]string(nil), base...)
	for _, entry := range env {
		if strings.HasPrefix(entry, "SHLVL=") {
			return env
		}
	}
	return append(env, "SHLVL=1")
}

func (c Client) Status(ctx context.Context) (Status, error) {
	encoded, err := c.run(ctx, "status", "--json")
	if err != nil {
		return Status{Present: true}, err
	}
	return ParseStatus(encoded)
}

// ServedEndpoint returns the HTTPS origin whose root handler proxies target.
// A different Serve handler is never adopted as Sessions remote access.
func (c Client) ServedEndpoint(ctx context.Context, target string) (string, error) {
	encoded, err := c.run(ctx, "serve", "status", "--json")
	if err != nil {
		return "", err
	}
	var status serveStatus
	if err := json.Unmarshal(encoded, &status); err != nil {
		return "", fmt.Errorf("decode Tailscale Serve status: %w", err)
	}
	wanted := normalizeTarget(target)
	for authority, web := range status.Web {
		handler, ok := web.Handlers["/"]
		if !ok || normalizeTarget(handler.Proxy) != wanted {
			continue
		}
		return endpointFromAuthority(authority), nil
	}
	return "", nil
}

func (c Client) Enable(ctx context.Context, target string) error {
	_, err := c.run(ctx, "serve", "--bg", target)
	return err
}

func (c Client) Disable(ctx context.Context) error {
	_, err := c.run(ctx, "serve", "--https=443", "--set-path=/", "off")
	return err
}

func normalizeTarget(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimSuffix(strings.TrimSpace(value), "/")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return strings.TrimSuffix(parsed.String(), "/")
}

func endpointFromAuthority(authority string) string {
	parsed, err := url.Parse("https://" + strings.TrimSpace(authority))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return "https://" + parsed.Host
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return text
}
