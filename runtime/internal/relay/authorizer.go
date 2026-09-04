package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

type Authorizer interface {
	Authorize(context.Context, relayauth.Response) error
}

type AllowListAuthorizer struct{ Path string }

type allowListDocument struct {
	Machines map[string]string `json:"machines"`
}

func (a AllowListAuthorizer) Authorize(_ context.Context, response relayauth.Response) error {
	encoded, err := os.ReadFile(a.Path)
	if err != nil {
		return fmt.Errorf("read relay allow-list: %w", err)
	}
	var document allowListDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		return fmt.Errorf("decode relay allow-list: %w", err)
	}
	expected := document.Machines[response.MachineID]
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(response.PublicKey)) != 1 {
		return errors.New("machine key is not allowed")
	}
	return nil
}

type DirectoryAuthorizer struct {
	URL        string
	OwnerToken string
	Client     *http.Client
}

func (a DirectoryAuthorizer) Authorize(ctx context.Context, response relayauth.Response) error {
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := strings.TrimSuffix(strings.TrimSpace(a.URL), "/") + "/api/machines/index"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.OwnerToken))
	serverResponse, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("query fleet directory: %w", err)
	}
	defer serverResponse.Body.Close()
	if serverResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("fleet directory returned HTTP %d", serverResponse.StatusCode)
	}
	var directory struct {
		Machines []struct {
			ID        string `json:"id"`
			PublicKey string `json:"machine_public_key"`
		} `json:"machines"`
	}
	decoder := json.NewDecoder(io.LimitReader(serverResponse.Body, 1<<20))
	if err := decoder.Decode(&directory); err != nil {
		return fmt.Errorf("decode fleet directory: %w", err)
	}
	for _, machine := range directory.Machines {
		if machine.ID == response.MachineID && subtle.ConstantTimeCompare([]byte(machine.PublicKey), []byte(response.PublicKey)) == 1 {
			return nil
		}
	}
	return errors.New("machine key is not in the owner's directory")
}

type AnyAuthorizer []Authorizer

func (authorizers AnyAuthorizer) Authorize(ctx context.Context, response relayauth.Response) error {
	var failures []string
	for _, authorizer := range authorizers {
		if err := authorizer.Authorize(ctx, response); err == nil {
			return nil
		} else {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) == 0 {
		return errors.New("relay has no machine-key authorizer")
	}
	return errors.New(strings.Join(failures, "; "))
}
