package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type fakeAccountUsage struct {
	sessionService
	calls    int
	refresh  []bool
	tool     string
	name     string
	deadline bool
	err      error
}

func (f *fakeAccountUsage) AccountUsage(ctx context.Context, tool, name string, refresh bool) ([]sessionruntime.AccountUsage, error) {
	f.calls++
	f.refresh = append(f.refresh, refresh)
	f.tool, f.name = tool, name
	_, f.deadline = ctx.Deadline()
	if f.err != nil {
		return nil, f.err
	}
	return []sessionruntime.AccountUsage{{
		Tool: "codex", Name: "work", State: sessionruntime.AccountUsageAvailable, CheckedAt: 5, ReadAt: 5,
		Buckets: []sessionruntime.AccountUsageBucket{{LimitID: "codex", Windows: []sessionruntime.AccountUsageWindow{{Kind: "primary", UsedPercent: 30}}}},
	}, {
		Tool: "claude", Name: "home", State: sessionruntime.AccountUsageUnsupported, Message: "Claude does not offer a supported way to read usage",
	}}, nil
}

func usageRequest(t *testing.T, server *Server, target string, principal authPrincipal) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, principal))
	response := httptest.NewRecorder()
	if !server.handleProfilesRoute(response, request, "") {
		t.Fatalf("%s not handled", target)
	}
	return response
}

// Any client may read the latest reading; only this computer or a paired host
// administrator may make the daemon ask the provider again on demand.
func TestAccountUsageRouteAnswersAndGatesRefresh(t *testing.T) {
	service := &fakeAccountUsage{}
	server := &Server{registry: service}
	response := usageRequest(t, server, "/api/account-usage?tool=codex&name=work", authPrincipal{})
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Accounts []sessionruntime.AccountUsage `json:"accounts"`
		TTL      int                           `json:"ttl_seconds"`
	}
	decodeBody(t, response, &body)
	if len(body.Accounts) != 2 || body.Accounts[0].Buckets[0].Windows[0].UsedPercent != 30 || body.Accounts[1].State != "unsupported" || body.TTL <= 0 {
		t.Fatalf("body = %#v", body)
	}
	if service.tool != "codex" || service.name != "work" || service.refresh[0] || !service.deadline {
		t.Fatalf("service saw tool=%q name=%q refresh=%v deadline=%v", service.tool, service.name, service.refresh, service.deadline)
	}
	if response = usageRequest(t, server, "/api/account-usage?refresh=1", authPrincipal{}); response.Code != http.StatusForbidden || service.calls != 1 {
		t.Fatalf("open-access refresh = %d, calls %d", response.Code, service.calls)
	}
	for _, principal := range []authPrincipal{{Local: true}, {HostAdmin: true}} {
		if response = usageRequest(t, server, "/api/account-usage?refresh=1", principal); response.Code != http.StatusOK || !service.refresh[len(service.refresh)-1] {
			t.Fatalf("authorized refresh = %d, refresh %v", response.Code, service.refresh)
		}
	}
}

func TestAccountUsageRouteExplainsFailures(t *testing.T) {
	server := &Server{registry: &fakeAccountUsage{err: errors.New("unknown account; `sessions accounts` lists the accounts on this computer")}}
	if response := usageRequest(t, server, "/api/account-usage?name=nope", authPrincipal{Local: true}); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown account = %d", response.Code)
	}
	if response := usageRequest(t, &Server{registry: &fakeAccountUsage{}}, "/api/account-usage?tool=gemini", authPrincipal{Local: true}); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown tool = %d", response.Code)
	}
	// An older registry without usage support answers with what to do.
	if response := usageRequest(t, &Server{registry: &fakeAccounts{}}, "/api/account-usage", authPrincipal{Local: true}); response.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported registry = %d", response.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/account-usage", nil)
	response := httptest.NewRecorder()
	(&Server{registry: &fakeAccountUsage{}}).handleProfilesRoute(response, request, "")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", response.Code)
	}
}

// Through the whole server a loopback client reaches the route.
func TestAccountUsageRouteThroughTheServer(t *testing.T) {
	daemon := newTestDaemon(t)
	service := &fakeAccountUsage{sessionService: daemon.registry}
	daemon.handler.registry = service
	response := serve(t, daemon.handler, http.MethodGet, "/api/account-usage?refresh=1", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK || service.calls != 1 || !service.refresh[0] {
		t.Fatalf("usage = %d %s", response.Code, response.Body.String())
	}
}
