package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

type apiClient struct {
	host           string
	port           string
	tokenPath      string
	localToken     bool
	client         *http.Client
	creatorSession string
	ownerID        string
}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func newAPIClient(host, port, tokenPath string, localToken bool) (*apiClient, error) {
	if port == "" {
		port = "8787"
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     60 * time.Second,
	}
	return &apiClient{
		host: host, port: port, tokenPath: tokenPath, localToken: localToken,
		client:         &http.Client{Transport: transport},
		creatorSession: os.Getenv("SESSIONS_SESSION_ID"),
		ownerID:        os.Getenv("SESSIONS_OWNER_ID"),
	}, nil
}

func cliHostIsLoopback(host string) bool {
	value := strings.TrimSpace(host)
	if strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return false
		}
		value = parsed.Hostname()
	} else if parsedHost, _, err := net.SplitHostPort(value); err == nil {
		value = parsedHost
	}
	value = strings.Trim(value, "[]")
	if strings.EqualFold(value, "localhost") {
		return true
	}
	ip := net.ParseIP(value)
	return ip != nil && ip.IsLoopback()
}

func (c *apiClient) close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.CloseIdleConnections()
}

func (c *apiClient) target(path string) (*url.URL, error) {
	if strings.HasPrefix(strings.ToLower(c.host), "http://") || strings.HasPrefix(strings.ToLower(c.host), "https://") {
		parsed, err := url.Parse(c.host)
		if err != nil {
			return nil, err
		}
		if parsed.Scheme != "https" {
			parsed.Scheme = "http"
		}
		if parsed.Hostname() == "" {
			return nil, fmt.Errorf("invalid sessionsd host %q", c.host)
		}
		if parsed.Port() == "" {
			parsed.Host = net.JoinHostPort(parsed.Hostname(), c.port)
		}
		parsed.Path = path
		parsed.RawPath = ""
		parsed.RawQuery = ""
		if index := strings.IndexByte(path, '?'); index >= 0 {
			parsed.Path = path[:index]
			parsed.RawQuery = path[index+1:]
		}
		return parsed, nil
	}
	return url.Parse("http://" + net.JoinHostPort(c.host, c.port) + path)
}

func (c *apiClient) websocketTarget(id string, extra url.Values) (*url.URL, error) {
	target, err := c.target("/ws")
	if err != nil {
		return nil, err
	}
	if target.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	query := target.Query()
	query.Set("sessionId", id)
	token, err := c.tokenForRequest()
	if err != nil {
		return nil, err
	}
	if token != "" {
		query.Set("token", token)
	}
	for key, values := range extra {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	target.RawQuery = query.Encode()
	return target, nil
}

func (c *apiClient) readToken() (string, error) {
	if c.tokenPath == "" {
		return "", nil
	}
	if c.localToken {
		return tokenstore.Read(c.tokenPath)
	}
	encoded, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(encoded)), nil
}

func (c *apiClient) tokenForRequest() (string, error) {
	if c.localToken {
		// Loopback is authorized by the immediate OS peer. Keep the master token
		// out of local HTTP headers and WebSocket URLs, and keep local control
		// available when only the remote-administrator token needs repair.
		return "", nil
	}
	return c.readToken()
}

func (c *apiClient) request(ctx context.Context, method, path string, body any, timeout time.Duration) (apiResponse, error) {
	return c.requestWithHeaders(ctx, method, path, body, timeout, nil)
}

func (c *apiClient) requestWithHeaders(
	ctx context.Context,
	method, path string,
	body any,
	timeout time.Duration,
	headers http.Header,
) (apiResponse, error) {
	target, err := c.target(path)
	if err != nil {
		return apiResponse{}, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := compactJSON(body)
		if err != nil {
			return apiResponse{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return apiResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	token, err := c.tokenForRequest()
	if err != nil {
		return apiResponse{}, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Sessions-Client", "sessions-cli")
	if (method == http.MethodPost && (path == "/api/sessions" || path == "/api/lanes" || path == "/api/sessions/end-batch")) || method == http.MethodDelete {
		// An external principal takes precedence when inherited from the
		// environment. Otherwise forward this CLI process's session ancestry.
		// DELETE needs the same provenance so agent-initiated termination is
		// attributable to the exact Sessions runtime that requested it.
		if c.ownerID != "" {
			request.Header.Set("X-Sessions-Owner-ID", c.ownerID)
		} else if c.creatorSession != "" {
			request.Header.Set("X-Sessions-Creator-Session", c.creatorSession)
		}
	}
	for name, values := range headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return apiResponse{}, err
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, err
	}
	return apiResponse{status: response.StatusCode, header: response.Header.Clone(), body: encoded}, nil
}

func (a *app) getJSON(path string, target any) error {
	response, err := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		if id := sessionIDFromAPIPath(path); id != "" {
			return fail(1, "%s", unknownSessionMessage(id))
		}
	}
	if response.status >= 400 {
		return fail(2, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	if err := json.Unmarshal(response.body, target); err != nil {
		return err
	}
	return nil
}

func (a *app) postJSON(path string, body, target any, errorCode int) error {
	return a.postJSONWithHeaders(path, body, target, errorCode, nil)
}

func (a *app) postJSONWithHeaders(path string, body, target any, errorCode int, headers http.Header) error {
	response, err := a.api.requestWithHeaders(context.Background(), http.MethodPost, path, body, 0, headers)
	if err != nil {
		return fail(errorCode, "%s → %s", path, err)
	}
	if response.status == http.StatusNotFound {
		if id := sessionIDFromAPIPath(path); id != "" {
			return fail(1, "%s", unknownSessionMessage(id))
		}
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return fail(errorCode, "%s", payload.Error)
		}
		return fail(errorCode, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	if target != nil {
		if err := json.Unmarshal(response.body, target); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) putJSON(path string, body, target any, errorCode int) error {
	response, err := a.api.request(context.Background(), http.MethodPut, path, body, 0)
	if err != nil {
		return fail(errorCode, "%s → %s", path, err)
	}
	if response.status == http.StatusNotFound {
		if id := sessionIDFromAPIPath(path); id != "" {
			return fail(1, "%s", unknownSessionMessage(id))
		}
	}
	if response.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.body, &payload) == nil && payload.Error != "" {
			return fail(errorCode, "%s", payload.Error)
		}
		return fail(errorCode, "%s → %d %s", path, response.status, prefixBytes(response.body, 200))
	}
	if target != nil {
		if err := json.Unmarshal(response.body, target); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) delete(path string, body any) (bool, error) {
	response, err := a.api.request(context.Background(), http.MethodDelete, path, body, 0)
	if err != nil {
		return false, err
	}
	if response.status >= 400 && response.status != http.StatusNotFound {
		return false, fail(2, "%s → %d", path, response.status)
	}
	return response.status == http.StatusOK, nil
}

func (a *app) getText(path string) (string, error) {
	response, err := a.api.request(context.Background(), http.MethodGet, path, nil, 0)
	if err != nil {
		return "", err
	}
	if response.status == http.StatusNotFound {
		if id := sessionIDFromAPIPath(path); id != "" {
			return "", fail(1, "%s", unknownSessionMessage(id))
		}
		return "", nil
	}
	if response.status >= 400 {
		return "", fail(2, "%s → %d", path, response.status)
	}
	return string(response.body), nil
}

func prefixBytes(value []byte, count int) string {
	if len(value) <= count {
		return string(value)
	}
	return string(value[:count])
}

func sessionIDFromAPIPath(path string) string {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if index := strings.IndexAny(rest, "/?"); index >= 0 {
		rest = rest[:index]
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return rest
	}
	return decoded
}

func escapeID(id string) string { return url.PathEscape(id) }
