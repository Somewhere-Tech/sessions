package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/relayauth"
)

func TestUnsignedDirectoryAuthorizerRefusesBeforeSendingToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests++
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"machines":[{"id":"fixture-machine","machine_public_key":"fixture-key"}]}`))
	}))
	defer server.Close()
	err := (DirectoryAuthorizer{URL: server.URL, OwnerToken: "fixture-owner-token"}).Authorize(
		context.Background(), relayauth.Response{MachineID: "fixture-machine", PublicKey: "fixture-key"},
	)
	if err == nil || !strings.Contains(err.Error(), "--allow-file") {
		t.Fatalf("unsigned directory authorization = %v, want explicit static allow-list instruction", err)
	}
	if requests != 0 {
		t.Fatalf("unsupported directory authorizer sent %d requests, want zero", requests)
	}
}
