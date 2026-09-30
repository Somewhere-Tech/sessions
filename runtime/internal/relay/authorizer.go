package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

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

var ErrDirectoryAuthorizationUnavailable = errors.New("directory-backed relay authorization is not available: the fleet directory requires a machine signature; configure --allow-file with the allowed machine public keys")

// Retain the configuration type for compatibility, but do not send an owner
// token to an endpoint this relay cannot authenticate to.
func (a DirectoryAuthorizer) Authorize(_ context.Context, _ relayauth.Response) error {
	return ErrDirectoryAuthorizationUnavailable
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
