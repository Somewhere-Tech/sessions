package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestRelayInstallArgumentsKeepSecretsOutOfArgv(t *testing.T) {
	options := relayInstallOptions{
		Listen: "127.0.0.1:8899", Cert: "/tls/cert.pem", Key: "/tls/key.pem",
		DirectoryURL: "https://directory.example", OwnerTokenFile: "/secrets/owner-token",
	}
	got := relayInstallArguments("/bin/sessions-relay", false, options)
	want := []string{
		"/bin/sessions-relay", "--listen", "127.0.0.1:8899",
		"--cert", "/tls/cert.pem", "--key", "/tls/key.pem",
		"--directory-url", "https://directory.example", "--owner-token-file", "/secrets/owner-token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relay arguments = %#v, want %#v", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "owner-secret") {
		t.Fatal("relay owner token leaked into argv")
	}
}

func TestRelayInstallArgumentsSupportBundledSessionsd(t *testing.T) {
	got := relayInstallArguments("/app/runtime/sessionsd", true, relayInstallOptions{Listen: "127.0.0.1:8899", AllowFile: "/allow.json"})
	want := []string{"/app/runtime/sessionsd", "--relay", "--listen", "127.0.0.1:8899", "--allow-file", "/allow.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessionsd relay arguments = %#v, want %#v", got, want)
	}
}

func TestRelayInstallRequiresMachineAuthorizer(t *testing.T) {
	args := []string{}
	_, err := parseRelayInstallOptions(&args)
	if err == nil || !strings.Contains(err.Error(), "--allow-file") {
		t.Fatalf("missing authorizer error = %v", err)
	}
}
