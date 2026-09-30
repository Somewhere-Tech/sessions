package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type fakeAccountLogins struct {
	sessionService
	calls int
}

func (f *fakeAccountLogins) StartAccountLogin(tool, profile string) (sessionruntime.AccountLoginStatus, error) {
	f.calls++
	return sessionruntime.AccountLoginStatus{ID: "test", Tool: tool, Profile: profile, State: "opening"}, nil
}
func (f *fakeAccountLogins) AccountLogin(id, code string, cancel bool) (sessionruntime.AccountLoginStatus, error) {
	f.calls++
	return sessionruntime.AccountLoginStatus{ID: id, State: "waiting"}, nil
}

func TestAccountLoginRequiresAuthorityForEveryMethod(t *testing.T) {
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, allowed := range []bool{false, true} {
			service := &fakeAccountLogins{}
			server := &Server{registry: service}
			request := httptest.NewRequest(method, "/api/account-logins/test", strings.NewReader(`{"code":"test"}`))
			request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{HostAdmin: allowed}))
			response := httptest.NewRecorder()
			if !server.handleProfilesRoute(response, request, "") {
				t.Fatal("route not handled")
			}
			if !allowed && (response.Code != 403 || service.calls != 0) {
				t.Fatal("unauthorized login access")
			}
			if allowed && (response.Code != 200 || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("authorized status=%d", response.Code)
			}
		}
	}
}

func TestAccountLoginIsNotShadowedByFleetAccountRoutes(t *testing.T) {
	daemon := newTestDaemon(t)
	service := &fakeAccountLogins{sessionService: daemon.registry}
	daemon.handler.registry = service
	response := serve(t, daemon.handler, "POST", "/api/account-logins", strings.NewReader(`{"tool":"claude","profile":"work"}`), "127.0.0.1:1", nil)
	if response.Code != 200 || service.calls != 1 {
		t.Fatalf("route status=%d body=%s", response.Code, response.Body.String())
	}
}
