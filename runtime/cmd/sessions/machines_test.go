package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

func TestMachineCommandUsesDaemonFleetRelayPath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{}})
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESSIONS_HOST", server.URL)
	if _, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "machine-mini", Name: "Mini", Endpoint: "http://10.0.0.2:8787", Transport: "nearby",
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--machine", "mini", "--json", "ls"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if path != "/api/fleet/machine-mini/api/sessions" {
		t.Fatalf("request path = %q", path)
	}
}

func TestMachineRegistryKeepsCredentialsOutOfMetadataAndUsesPrivateModes(t *testing.T) {
	home := t.TempDir()
	machine, err := saveMachine(home, savedMachine{
		MachineID: "11111111-1111-4111-8111-111111111111",
		Name:      "Mac mini", Endpoint: "http://192.168.1.20:8787",
		Transport: "nearby", DeviceID: "22222222-2222-4222-8222-222222222222",
		ConnectedAt: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	}, "secret-device-token")
	if err != nil {
		t.Fatal(err)
	}
	if machine.Alias != "mac-mini" {
		t.Fatalf("alias = %q", machine.Alias)
	}
	metadata, err := os.ReadFile(machineRegistryPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte("secret-device-token")) {
		t.Fatal("machine registry contains the credential")
	}
	for _, path := range []string{
		machineRegistryPath(home),
		savedMachineTokenPath(home, machine.MachineID),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode(%s) = %04o", path, info.Mode().Perm())
		}
	}
}

func TestNativeMachineSyncSharesApprovedMachinesWithAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	payload := `{"machines":[{"machineId":"native-machine","name":"Mac mini","endpoint":"https://mini.example.ts.net","deviceId":"native-device","token":"native-device-token"}]}`
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json", "machines", "sync-native"}, strings.NewReader(payload), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("sync exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	registry, err := readMachineRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Machines) != 1 || registry.Machines[0].Alias != "mac-mini" || registry.Machines[0].Source != nativeAppMachineSource {
		t.Fatalf("registry = %#v", registry)
	}
	token, err := tokenstore.ReadSecret(savedMachineTokenPath(home, "native-machine"))
	if err != nil || token != "native-device-token" {
		t.Fatalf("saved token=%q err=%v", token, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--json", "machines", "sync-native"}, strings.NewReader(`{"machines":[]}`), &stdout, &stderr); code != 0 {
		t.Fatalf("clear exit=%d stderr=%q", code, stderr.String())
	}
	registry, err = readMachineRegistry(home)
	if err != nil || len(registry.Machines) != 0 {
		t.Fatalf("cleared registry=%#v err=%v", registry, err)
	}
}

func TestSavedMachineGlobalUsesLocalDaemonRelayUnlessDirect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	machine, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "33333333-3333-4333-8333-333333333333",
		Name: "Mini", Endpoint: "https://mini.example.ts.net",
		Transport: "tailnet", DeviceID: "44444444-4444-4444-8444-444444444444",
		ConnectedAt: time.Now(),
	}, "secret-device-token")
	if err != nil {
		t.Fatal(err)
	}
	application, err := newApp(
		[]string{"--machine", "mini", "ls"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	if application.api.host != "127.0.0.1" {
		t.Fatalf("api host = %q", application.api.host)
	}
	if application.api.pathPrefix != "/api/fleet/"+machine.MachineID || application.api.relayEndpoint != machine.Endpoint {
		t.Fatalf("relay client = host %q prefix %q endpoint %q", application.api.host, application.api.pathPrefix, application.api.relayEndpoint)
	}
	application.close()
	application, err = newApp(
		[]string{"--direct", "--machine", "mini", "ls"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	if application.api.host != machine.Endpoint || application.api.tokenPath != savedMachineTokenPath(home, machine.MachineID) {
		t.Fatalf("direct client = host %q token %q", application.api.host, application.api.tokenPath)
	}
}

func TestRawRemoteHostNeverReusesLocalDaemonToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Derive the local token location instead of spelling out the Unix layout,
	// or on Windows this plants the decoy somewhere the CLI never looks and the
	// test passes without exercising anything.
	localToken := filepath.Join(sessionstate.UserStateRootFor(home), "token")
	if err := writePrivateFile(localToken, []byte("local-master-token\n")); err != nil {
		t.Fatal(err)
	}
	application, err := newApp(
		[]string{"--host", "https://untrusted.example", "ls"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	token, tokenErr := application.api.readToken()
	if tokenErr != nil {
		t.Fatal(tokenErr)
	}
	if application.api.tokenPath != "" || token != "" {
		t.Fatalf("remote raw host inherited local credential path %q", application.api.tokenPath)
	}
}

func TestMachineEndpointValidationIsFailClosed(t *testing.T) {
	for _, test := range []struct {
		raw       string
		transport string
		ok        bool
	}{
		{"http://192.168.1.20:8787", "nearby", true},
		{"http://10.0.0.2:9000", "nearby", true},
		{"https://mini.example.ts.net", "tailnet", true},
		{"http://127.0.0.1:8787", "nearby", true},
		{"http://example.com:8787", "", false},
		{"http://192.168.1.20", "", false},
		{"https://example.com", "", false},
		{"https://mini.example.ts.net/path", "", false},
		{"ftp://192.168.1.20:8787", "", false},
	} {
		_, transport, err := validateMachineEndpoint(test.raw)
		if (err == nil) != test.ok || transport != test.transport {
			t.Errorf("validateMachineEndpoint(%q) transport=%q err=%v", test.raw, transport, err)
		}
	}
}

func TestForgetMachineRemovesOnlyLocalCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	machine, err := saveMachine(home, savedMachine{
		Alias: "mini", MachineID: "55555555-5555-4555-8555-555555555555",
		Name: "Mini", Endpoint: "http://192.168.1.20:8787",
		Transport: "nearby", DeviceID: "66666666-6666-4666-8666-666666666666",
		ConnectedAt: time.Now(),
	}, "secret-device-token")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"machines", "forget", "mini"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(savedMachineTokenPath(home, machine.MachineID)); !os.IsNotExist(err) {
		t.Fatalf("token still exists: %v", err)
	}
	registry, err := readMachineRegistry(home)
	if err != nil || len(registry.Machines) != 0 {
		t.Fatalf("registry=%#v err=%v", registry, err)
	}
	if !strings.Contains(stdout.String(), "Run `sessions devices revoke") {
		t.Fatalf("missing host revocation warning: %q", stdout.String())
	}
}

func TestClientIDIsStableAndPrivate(t *testing.T) {
	home := t.TempDir()
	first, err := loadOrCreateClientID(home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateClientID(home)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("client ids = %q, %q", first, second)
	}
	info, err := os.Stat(filepath.Clean(clientIDPath(home)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("client id mode = %04o", info.Mode().Perm())
	}
}

func TestMachineIDValidationKeepsCredentialsInsideTheClientDirectory(t *testing.T) {
	escape := "../../../../../../tmp/sessions-escape/probe"
	for _, hostile := range []string{
		"", "   ", escape, "..", ".", ".hidden", "a/b", `a\b`,
		"has space", "id\r\n", "id\x00", strings.Repeat("x", maxMachineIDLength+1),
	} {
		if err := validateMachineID(hostile); err == nil {
			t.Fatalf("validateMachineID(%q) accepted a hostile id", hostile)
		}
	}
	for _, allowed := range []string{
		"11111111-1111-4111-8111-111111111111", "native-machine", "mac_mini.local", "a",
	} {
		if err := validateMachineID(allowed); err != nil {
			t.Fatalf("validateMachineID(%q) = %v", allowed, err)
		}
	}

	home := t.TempDir()
	if _, err := saveMachine(home, savedMachine{
		MachineID: escape, Name: "Hostile", Endpoint: "http://192.168.1.20:8787",
		Transport: "nearby", DeviceID: "device", ConnectedAt: time.Now(),
	}, "device-token"); err == nil {
		t.Fatal("saveMachine accepted a path-escaping machine id")
	}
	if _, err := os.Stat(savedMachineTokenPath(home, escape)); !os.IsNotExist(err) {
		t.Fatalf("credential written outside the client directory: %v", err)
	}
	if _, err := os.Stat(machineRegistryPath(home)); !os.IsNotExist(err) {
		t.Fatal("registry was written for a rejected machine id")
	}
}

func TestNativeMachineSyncRejectsPathEscapingMachineIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	escape := "../../../../../../tmp/sessions-escape/native"
	payload := `{"machines":[{"machineId":"` + escape + `","name":"Hostile","endpoint":"https://mini.example.ts.net","deviceId":"d","token":"t"}]}`
	var stdout, stderr bytes.Buffer
	code := run([]string{"machines", "sync-native"}, strings.NewReader(payload), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("sync-native exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid stable id") || !strings.Contains(stderr.String(), "parent-directory reference") {
		t.Fatalf("sync-native stderr=%q", stderr.String())
	}
	if _, err := os.Stat(savedMachineTokenPath(home, escape)); !os.IsNotExist(err) {
		t.Fatalf("sync-native wrote a credential outside the client directory: %v", err)
	}
}

func TestForgetSkipsCredentialRemovalForAPoisonedRegistryEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	victim := filepath.Join(home, "victim.token")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A registry written by an older, unvalidated build must not turn forget
	// into an arbitrary file deletion.
	poisoned := machineRegistry{Version: machineRegistryVersion, Machines: []savedMachine{{
		Alias: "poisoned", MachineID: "../../../../victim", Name: "Poisoned",
		Endpoint: "http://192.168.1.20:8787", Transport: "nearby", DeviceID: "device",
		ConnectedAt: time.Now().UTC(),
	}}}
	if err := writeMachineRegistry(home, poisoned); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"machines", "forget", "poisoned"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("forget exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("forget deleted a file outside the client directory: %v", err)
	}
	registry, err := readMachineRegistry(home)
	if err != nil || len(registry.Machines) != 0 {
		t.Fatalf("registry = %#v err=%v", registry, err)
	}
}
