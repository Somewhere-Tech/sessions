package api

import (
	"encoding/json"
	"net/http"
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
	wrongMethod := serve(t, daemon.handler, http.MethodPut, "/api/profiles/claude/work", nil, "127.0.0.1:1", nil)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on an account = %d", wrongMethod.Code)
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
