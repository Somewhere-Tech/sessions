package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

// fakeAccounts is the registry side of the account routes: what it records is
// what the handler passed it, so the test describes the route rather than the
// manager, which has its own tests.
type fakeAccounts struct {
	sessionService
	created   []sessionruntime.ProfileStatus
	forgotten []string
	renamed   []string
	createErr error
	forgetErr error
}

func (f *fakeAccounts) CreateAccount(tool, name, label string) (sessionruntime.ProfileStatus, error) {
	if f.createErr != nil {
		return sessionruntime.ProfileStatus{}, f.createErr
	}
	profile := sessionruntime.ProfileStatus{
		Tool: tool, Name: name, Label: label,
		Path: "/state/profiles/" + tool + "/" + name, Sessions: []sessionruntime.ProfileSession{},
	}
	f.created = append(f.created, profile)
	return profile, nil
}

func (f *fakeAccounts) ForgetAccount(tool, name string) error {
	if f.forgetErr != nil {
		return f.forgetErr
	}
	f.forgotten = append(f.forgotten, tool+"/"+name)
	return nil
}

func (f *fakeAccounts) RenameAccount(tool, name, label string) (sessionruntime.ProfileStatus, error) {
	if err := sessionruntime.ValidateAccountLabel(label); err != nil {
		return sessionruntime.ProfileStatus{}, err
	}
	f.renamed = append(f.renamed, tool+"/"+name+"="+label)
	return sessionruntime.ProfileStatus{Tool: tool, Name: name, Label: label, Sessions: []sessionruntime.ProfileSession{}}, nil
}

func (f *fakeAccounts) AccountHomePath(tool, name string) string {
	return "/state/profiles/" + tool + "/" + name
}

func TestCreatingAnAccountAnswersWithIt(t *testing.T) {
	daemon := newTestDaemon(t)
	accounts := &fakeAccounts{sessionService: daemon.registry}
	daemon.handler.registry = accounts

	response := serve(t, daemon.handler, http.MethodPost, "/api/profiles",
		strings.NewReader(`{"tool":"claude","name":"work","label":"Work — team plan"}`), "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("create account = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Profile sessionruntime.ProfileStatus `json:"profile"`
	}
	decodeBody(t, response, &body)
	if body.Profile.Tool != "claude" || body.Profile.Name != "work" || body.Profile.Label != "Work — team plan" {
		t.Fatalf("created account = %#v", body.Profile)
	}
	if len(accounts.created) != 1 {
		t.Fatalf("the registry recorded %d accounts", len(accounts.created))
	}
}

// Forgetting says what it did and what it left, because what it left is a
// subscription's login and history.
func TestForgettingAnAccountSaysTheHomeWasLeft(t *testing.T) {
	daemon := newTestDaemon(t)
	accounts := &fakeAccounts{sessionService: daemon.registry}
	daemon.handler.registry = accounts

	response := serve(t, daemon.handler, http.MethodDelete, "/api/profiles/claude/work", nil, "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("forget account = %d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	decodeBody(t, response, &body)
	if body["forgotten"] != "claude/work" || body["home"] != "/state/profiles/claude/work" {
		t.Fatalf("forget answer = %#v", body)
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "left in place") {
		t.Fatalf("forget answer does not say the home was left: %#v", body)
	}
	if len(accounts.forgotten) != 1 || accounts.forgotten[0] != "claude/work" {
		t.Fatalf("registry forgot %#v", accounts.forgotten)
	}
}

func TestAccountRoutesRefuseWhatTheyCannotDo(t *testing.T) {
	daemon := newTestDaemon(t)
	accounts := &fakeAccounts{sessionService: daemon.registry}
	daemon.handler.registry = accounts

	bad := serve(t, daemon.handler, http.MethodPost, "/api/profiles", strings.NewReader("not json"), "127.0.0.1:1", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid body = %d", bad.Code)
	}
	wrongMethod := serve(t, daemon.handler, http.MethodPatch, "/api/profiles/claude/work", nil, "127.0.0.1:1", nil)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH on an account = %d", wrongMethod.Code)
	}
	// The listing still answers, unchanged.
	listing := serve(t, daemon.handler, http.MethodGet, "/api/profiles", nil, "127.0.0.1:1", nil)
	if listing.Code != http.StatusOK && listing.Code != http.StatusNotImplemented {
		t.Fatalf("profile listing = %d %s", listing.Code, listing.Body.String())
	}
}

// The listing carries the two account facts on the wire, so a client does not
// have to guess a label or infer a login from a session that happens to exist.
func TestProfileListingCarriesLabelAndSignedIn(t *testing.T) {
	encoded, err := json.Marshal(sessionruntime.ProfileStatus{
		Tool: "claude", Name: "work", Label: "Work", SignedIn: true,
		Sessions: []sessionruntime.ProfileSession{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["label"] != "Work" || wire["signed_in"] != true {
		t.Fatalf("profile on the wire = %#v", wire)
	}
}

// Renaming is an account decision: only this computer or a paired host
// administrator may make it, and the handler passes the trimmed nickname on.
func TestRenamingAnAccountRequiresAuthority(t *testing.T) {
	for _, principal := range []authPrincipal{{}, {Local: true}, {HostAdmin: true}} {
		accounts := &fakeAccounts{}
		server := &Server{registry: accounts}
		request := httptest.NewRequest(http.MethodPut, "/api/profiles/claude/work", strings.NewReader(`{"label":"  Team  "}`))
		request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, principal))
		response := httptest.NewRecorder()
		if !server.handleProfilesRoute(response, request, "") {
			t.Fatal("route not handled")
		}
		allowed := principal.Local || principal.HostAdmin
		if !allowed && (response.Code != http.StatusForbidden || len(accounts.renamed) != 0) {
			t.Fatalf("open-access rename = %d, renamed %v", response.Code, accounts.renamed)
		}
		if allowed && (response.Code != http.StatusOK || len(accounts.renamed) != 1 || accounts.renamed[0] != "claude/work=Team") {
			t.Fatalf("authorized rename = %d %s, renamed %v", response.Code, response.Body.String(), accounts.renamed)
		}
	}
}

func TestRenamingAnAccountExplainsABadRequest(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"label":"line\nbreak"}`} {
		accounts := &fakeAccounts{}
		server := &Server{registry: accounts}
		request := httptest.NewRequest(http.MethodPut, "/api/profiles/claude/work", strings.NewReader(body))
		request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{Local: true}))
		response := httptest.NewRecorder()
		server.handleProfilesRoute(response, request, "")
		if response.Code != http.StatusBadRequest || len(accounts.renamed) != 0 {
			t.Fatalf("rename with %s = %d, renamed %v", body, response.Code, accounts.renamed)
		}
	}
}

// Through the whole server, a loopback client is local and may rename.
func TestLocalClientRenamesAnAccountThroughTheServer(t *testing.T) {
	daemon := newTestDaemon(t)
	accounts := &fakeAccounts{sessionService: daemon.registry}
	daemon.handler.registry = accounts
	response := serve(t, daemon.handler, http.MethodPut, "/api/profiles/codex/personal",
		strings.NewReader(`{"label":"Personal"}`), "127.0.0.1:1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Profile sessionruntime.ProfileStatus `json:"profile"`
	}
	decodeBody(t, response, &body)
	if body.Profile.Name != "personal" || body.Profile.Label != "Personal" {
		t.Fatalf("renamed account = %#v", body.Profile)
	}
}
