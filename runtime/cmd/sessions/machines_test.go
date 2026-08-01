package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/tokenstore"
)

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

func TestSavedMachineGlobalSelectsEndpointAndTokenWithoutPrintingIt(t *testing.T) {
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
	if application.api.host != machine.Endpoint {
		t.Fatalf("api host = %q", application.api.host)
	}
	if application.api.tokenPath != savedMachineTokenPath(home, machine.MachineID) {
		t.Fatalf("token path = %q", application.api.tokenPath)
	}
}

func TestRawRemoteHostNeverReusesLocalDaemonToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localToken := filepath.Join(home, ".local", "state", "sessions", "token")
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
		{"http://127.0.0.1:8787", "", false},
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
