package fleetaccount

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxResponseBody = 1 << 20

type cloudClient struct {
	mu      sync.Mutex
	baseURL string
	store   *store
	client  *http.Client
	now     func() time.Time
}

func newCloudClient(baseURL string, state *store, client *http.Client) (*cloudClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("SESSIONS_FLEET_URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("SESSIONS_FLEET_URL must use HTTP or HTTPS")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &cloudClient{
		baseURL: strings.TrimSuffix(parsed.String(), "/"), store: state, client: client, now: time.Now,
	}, nil
}

func (c *cloudClient) request(
	ctx context.Context, method, path string, body any, authenticate bool,
	machineID string, key ed25519.PrivateKey,
) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	encoded, err := encodeRequestBody(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if len(encoded) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticate {
		if err := c.authorize(request); err != nil {
			return nil, err
		}
	}
	if key != nil {
		if err := signRequest(request, encoded, machineID, key, c.now()); err != nil {
			return nil, err
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("contact Somewhere fleet service: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("read Somewhere fleet response: %w", err)
	}
	if len(responseBody) > maxResponseBody {
		return nil, errors.New("Somewhere fleet response is unexpectedly large")
	}
	if authenticate {
		if err := c.store.applyRotation(response.Header); err != nil {
			return nil, err
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, cloudResponseError(response.StatusCode, responseBody)
	}
	return responseBody, nil
}

func (c *cloudClient) authorize(request *http.Request) error {
	state, err := c.store.load()
	if err != nil {
		return err
	}
	if !state.signedIn() {
		return errors.New("not signed in to Somewhere")
	}
	request.Header.Set("Authorization", "Bearer "+state.Tokens.AccessToken)
	request.Header.Set("X-Refresh-Token", state.Tokens.RefreshToken)
	return nil
}

func encodeRequestBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Somewhere fleet request: %w", err)
	}
	return encoded, nil
}

func cloudResponseError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	if response.Error == "" {
		response.Error = strings.TrimSpace(string(body))
	}
	if response.Error == "" {
		response.Error = http.StatusText(status)
	}
	return fmt.Errorf("Somewhere fleet service returned %d: %s", status, response.Error)
}
