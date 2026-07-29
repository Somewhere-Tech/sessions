package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLANCLILifecycle(t *testing.T) {
	var enabled bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/health" && request.Method == http.MethodGet:
			_, _ = io.WriteString(response, `{"ok":true,"name":"sessionsd"}`)
		case request.URL.Path == "/api/lan" && request.Method == http.MethodGet:
			var endpoint any
			if enabled {
				endpoint = serverURL(request)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"enabled": enabled, "url": endpoint})
		case request.URL.Path == "/api/lan" && request.Method == http.MethodPost:
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			enabled = body.Enabled
			var endpoint any
			if enabled {
				endpoint = serverURL(request)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"enabled": enabled, "url": endpoint})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	previousPicker := primaryLANIPv4
	primaryLANIPv4 = func() (net.IP, error) { return net.ParseIP("127.0.0.1"), nil }
	t.Cleanup(func() { primaryLANIPv4 = previousPicker })

	for _, test := range []struct {
		name string
		want string
	}{
		{name: "enable", want: "LAN access verified (HTTP 200)."},
		{name: "status", want: "LAN access verified (HTTP 200)."},
		{name: "disable", want: "LAN access disabled."},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"--host", server.URL, "lan", test.name}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout=%q, want %q", stdout.String(), test.want)
			}
			if test.name == "enable" {
				for _, want := range []string{"run `sessions pair` and scan its one-time QR", "LAN access works only on this network. From anywhere: sessions remote enable"} {
					if !strings.Contains(stdout.String(), want) {
						t.Fatalf("enable output missing %q: %q", want, stdout.String())
					}
				}
				if strings.Contains(stdout.String(), "token=") {
					t.Fatalf("QR/output leaked a token in the URL: %q", stdout.String())
				}
			}
		})
	}
}

func TestLANStatusReportsChangedInterface(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{"enabled": true, "url": "http://192.168.1.20:8787"})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	previousPicker := primaryLANIPv4
	primaryLANIPv4 = func() (net.IP, error) { return net.ParseIP("192.168.2.30"), nil }
	t.Cleanup(func() { primaryLANIPv4 = previousPicker })
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "lan", "status"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "the current LAN IP no longer matches the listener") || !strings.Contains(stderr.String(), "sessions lan enable") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestLANCLIUnreachableDaemonTeachesFix(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", endpoint, "lan", "enable"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "Start it first with `sessions install`") || !strings.Contains(stderr.String(), "sessions lan enable") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
