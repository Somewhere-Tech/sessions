package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenCommandPrintsDaemonToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot := filepath.Join(home, "state")
	t.Setenv("SESSIONS_STATE_DIR", stateRoot)
	tokenPath := filepath.Join(stateRoot, "token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	application, err := newApp([]string{"token"}, strings.NewReader(""), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != token+"\n" {
		t.Fatalf("sessions token output = %q", got)
	}
}

func TestLoopbackRequestsDoNotDependOnTheMasterTokenFile(t *testing.T) {
	client := apiClient{
		tokenPath:  filepath.Join(t.TempDir(), "damaged-token"),
		localToken: true,
	}
	if err := os.WriteFile(client.tokenPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := client.tokenForRequest()
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Fatalf("loopback request inherited master token %q", token)
	}
}
