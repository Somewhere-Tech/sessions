package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	endpoint *url.URL
	token    string
	http     *http.Client
}

func NewClient(endpoint, token string) (*Client, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid target endpoint %q", endpoint)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("target endpoint must use http or https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("target endpoint must not include a path, query, or fragment")
	}
	parsed.Path = ""
	return &Client{
		endpoint: parsed, token: token,
		http: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

func (c *Client) Endpoint() string { return strings.TrimSuffix(c.endpoint.String(), "/") }

func (c *Client) Receive(ctx context.Context, body ReceiveRequest) (ReceiveResult, error) {
	var result ReceiveResult
	if err := c.post(ctx, "/api/migrate/receive", body, http.StatusCreated, &result); err != nil {
		return ReceiveResult{}, err
	}
	return result, nil
}

func (c *Client) Create(ctx context.Context, body ReceiveRequest) (CreateResult, error) {
	requestBody := CreateRequestFromReceive(body)
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return CreateResult{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.Endpoint()+"/api/migrate/create", bytes.NewReader(encoded),
	)
	if err != nil {
		return CreateResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return CreateResult{}, fmt.Errorf("target /api/migrate/create: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusAccepted {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return CreateResult{}, fmt.Errorf(
			"target /api/migrate/create returned %d: %s",
			response.StatusCode, strings.TrimSpace(string(detail)),
		)
	}
	var result CreateResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return CreateResult{}, fmt.Errorf("decode target /api/migrate/create: %w", err)
	}
	if result.Session.ID == "" {
		return CreateResult{}, errors.New("target create response did not include an id")
	}
	return result, nil
}

func (c *Client) post(ctx context.Context, path string, body any, wantStatus int, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint()+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("target %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("target %s returned %d: %s", path, response.StatusCode, strings.TrimSpace(string(detail)))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode target %s: %w", path, err)
	}
	return nil
}
