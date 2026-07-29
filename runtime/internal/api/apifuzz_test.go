package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestJSONBodiesRejectMalformedAndAmbiguousInput(t *testing.T) {
	daemon := newTestDaemon(t)
	deep := []byte(`{"data":` + strings.Repeat("[", 10_001) + "0" + strings.Repeat("]", 10_001) + "}")
	tests := []struct {
		name string
		body []byte
	}{
		{name: "truncated", body: []byte(`{"data":"unterminated`)},
		{name: "wrong type", body: []byte(`{"data":-9223372036854775809}`)},
		{name: "trailing garbage", body: []byte(`{"data":"ok"} garbage`)},
		{name: "multiple values", body: []byte(`{"data":"ok"}{}`)},
		{name: "non UTF-8", body: []byte{'{', '"', 'd', 'a', 't', 'a', '"', ':', '"', 0xff, '"', '}'}},
		{name: "excessive nesting", body: deep},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serve(t, daemon.handler, http.MethodPost, "/api/sessions/missing/input", bytes.NewReader(test.body), "127.0.0.1:1", nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			decodeBody(t, response, &body)
			if body["error"] == "" {
				t.Fatalf("missing parse error: %#v", body)
			}
		})
	}
}

func TestStaticSymlinkCannotEscapeWebRoot(t *testing.T) {
	daemon := newTestDaemon(t)
	outside := filepath.Join(daemon.root, "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("must not be served"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(daemon.config.WebDir, "escape.txt")); err != nil {
		t.Fatal(err)
	}

	response := serve(t, daemon.handler, http.MethodGet, "/escape.txt", nil, "198.51.100.1:1", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "must not be served") {
		t.Fatalf("static response escaped web root: %q", response.Body.String())
	}
}

func TestUploadSymlinkCannotEscapeHome(t *testing.T) {
	fixture := t.TempDir()
	home := filepath.Join(fixture, "home")
	outside := filepath.Join(fixture, "outside")
	mustMkdirAll(t, filepath.Join(home, ".local", "state", "sessions"))
	mustMkdirAll(t, outside)
	if err := os.Symlink(outside, filepath.Join(home, ".local", "state", "sessions", "uploads")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.registry.RequestKill(context.Background(), created.ID, false) })

	response := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/upload", strings.NewReader("secret"), "127.0.0.1:1", http.Header{
		"X-Sessions-Filename": {"../../escape.txt"},
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload escaped home: %#v", entries)
	}
}

type finiteFillReader struct {
	remaining int64
	read      int64
}

func (r *finiteFillReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	count := int64(len(buffer))
	if count > r.remaining {
		count = r.remaining
	}
	for index := int64(0); index < count; index++ {
		buffer[index] = 'x'
	}
	r.remaining -= count
	r.read += count
	return int(count), nil
}

func TestOversizedUploadStopsAtLimit(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	mustMkdirAll(t, home)
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: daemon.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.registry.RequestKill(context.Background(), created.ID, false) })
	body := &finiteFillReader{remaining: maxUploadBody + 1_024}

	response := serve(t, daemon.handler, http.MethodPost, "/api/sessions/"+created.ID+"/upload", body, "127.0.0.1:1", nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body.read != maxUploadBody+1 {
		t.Fatalf("handler read %d bytes, want exactly bounded probe %d", body.read, maxUploadBody+1)
	}
}

func TestMalformedWebSocketUpgradesReturn4xx(t *testing.T) {
	daemon := newTestDaemon(t)
	tests := []struct {
		method  string
		headers http.Header
	}{
		{method: http.MethodGet, headers: http.Header{}},
		{method: http.MethodGet, headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}},
		{method: http.MethodGet, headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-WebSocket-Version": {"12"}, "Sec-WebSocket-Key": {"bad"}}},
		{method: http.MethodGet, headers: http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"not-websocket"}, "Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {"not-base64"}}},
		{method: http.MethodPost, headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}},
	}
	for index, test := range tests {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			response := serve(t, daemon.handler, test.method, "/ws?mux=1", nil, "127.0.0.1:1", test.headers)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestConcurrentSessionHandlers(t *testing.T) {
	daemon := newTestDaemon(t)
	const workers = 8
	const rounds = 4
	errorsSeen := make(chan error, workers*rounds*8)
	var workersDone sync.WaitGroup
	start := make(chan struct{})

	request := func(method, target string, body io.Reader) *httptest.ResponseRecorder {
		httpRequest := httptest.NewRequest(method, target, body)
		httpRequest.RemoteAddr = "127.0.0.1:1"
		response := httptest.NewRecorder()
		daemon.handler.ServeHTTP(response, httpRequest)
		return response
	}
	for worker := 0; worker < workers; worker++ {
		workersDone.Add(1)
		go func(worker int) {
			defer workersDone.Done()
			<-start
			for round := 0; round < rounds; round++ {
				encoded := fmt.Sprintf(`{"cmd":"/bin/sh","cwd":%q,"name":"race-%d-%d"}`, daemon.root, worker, round)
				created := request(http.MethodPost, "/api/sessions", strings.NewReader(encoded))
				if created.Code != http.StatusCreated {
					errorsSeen <- fmt.Errorf("create status=%d body=%s", created.Code, created.Body.String())
					continue
				}
				var info state.SessionInfo
				if err := json.Unmarshal(created.Body.Bytes(), &info); err != nil || info.ID == "" {
					errorsSeen <- fmt.Errorf("decode create: id=%q err=%v", info.ID, err)
					continue
				}

				var actions sync.WaitGroup
				for _, action := range []struct {
					method string
					target string
					body   string
					ok     map[int]bool
				}{
					{method: http.MethodGet, target: "/api/sessions", ok: map[int]bool{http.StatusOK: true}},
					{method: http.MethodGet, target: "/api/sessions/" + info.ID + "/snapshot?cols=80", ok: map[int]bool{http.StatusOK: true}},
					{method: http.MethodGet, target: "/api/sessions/" + info.ID + "/events?tail=2", ok: map[int]bool{http.StatusOK: true}},
					{method: http.MethodPost, target: "/api/sessions/" + info.ID + "/input", body: `{"data":"x"}`, ok: map[int]bool{http.StatusOK: true, http.StatusNotFound: true}},
					{method: http.MethodDelete, target: "/api/sessions/" + info.ID, ok: map[int]bool{http.StatusOK: true}},
				} {
					action := action
					actions.Add(1)
					go func() {
						defer actions.Done()
						var body io.Reader
						if action.body != "" {
							body = strings.NewReader(action.body)
						}
						response := request(action.method, action.target, body)
						if !action.ok[response.Code] {
							errorsSeen <- fmt.Errorf("%s %s status=%d body=%s", action.method, action.target, response.Code, response.Body.String())
						}
					}()
				}
				actions.Wait()
			}
		}(worker)
	}
	close(start)
	workersDone.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	for _, info := range daemon.registry.List(false) {
		_ = daemon.registry.RequestKill(context.Background(), info.ID, false)
	}
}

type fuzzSessionService struct {
	session *state.Session
	info    state.SessionInfo
}

func (s *fuzzSessionService) Uptime() time.Duration { return time.Second }
func (s *fuzzSessionService) IsDiscovering() bool   { return false }
func (s *fuzzSessionService) Create(context.Context, state.CreateSessionRequest) (state.SessionInfo, error) {
	return state.SessionInfo{}, errors.New("create disabled in fuzz harness")
}
func (s *fuzzSessionService) List(bool) []state.SessionInfo {
	return []state.SessionInfo{s.info}
}
func (s *fuzzSessionService) Get(id string) (*state.Session, bool) {
	return s.session, id == "known"
}
func (s *fuzzSessionService) RequestKill(context.Context, string, bool) error { return nil }
func (s *fuzzSessionService) Input(context.Context, string, string) bool      { return true }
func (s *fuzzSessionService) DeepDiagnostics() []map[string]any               { return []map[string]any{} }
func (s *fuzzSessionService) UpdateTags(_ string, tags map[string]string) (map[string]string, error) {
	return state.CloneTags(tags), nil
}
func (s *fuzzSessionService) Tags(string) (map[string]string, error) { return nil, nil }

type fuzzPushService struct{}

func (*fuzzPushService) VAPIDPublicKey() (string, error) { return "fuzz-key", nil }
func (*fuzzPushService) AddSubscription(any) error       { return nil }
func (*fuzzPushService) RemoveSubscription(string) error { return nil }

type countingReader struct {
	reader io.Reader
	read   int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.read += int64(count)
	return count, err
}

func FuzzRequestRouting(f *testing.F) {
	root := f.TempDir()
	home := filepath.Join(root, "home")
	webDir := filepath.Join(root, "web")
	for _, path := range []string{home, webDir, filepath.Join(root, "state")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			f.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("fuzz ui"), 0o600); err != nil {
		f.Fatal(err)
	}
	f.Setenv("HOME", home)
	config := state.Config{
		Host: "127.0.0.1", Port: 8787, DefaultShell: "/bin/sh", DefaultCwd: root,
		DefaultCols: 80, DefaultRows: 24, StateRoot: filepath.Join(root, "state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), TokenPath: filepath.Join(root, "state", "token"),
		OpenPath: filepath.Join(root, "state", "open"), LaunchAgentsDir: filepath.Join(root, "agents"), WebDir: webDir,
	}
	if err := os.WriteFile(config.TokenPath, []byte(testToken), 0o600); err != nil {
		f.Fatal(err)
	}
	launcher := prototest.NewLauncher()
	registry := state.NewRegistry(config, launcher)
	info, err := registry.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		f.Fatal(err)
	}
	session, ok := registry.Get(info.ID)
	if !ok {
		f.Fatal("fuzz fixture session was not registered")
	}
	f.Cleanup(func() { _ = registry.RequestKill(context.Background(), info.ID, false) })
	handler := New(config, &fuzzSessionService{session: session, info: info}, &fuzzPushService{})

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		f.Fatal(err)
	}
	seeds := []struct {
		method, target, body, remote, auth, origin, filename string
		upgrade                                              bool
	}{
		{method: http.MethodGet, target: "/api/health", remote: "198.51.100.1:9"},
		{method: http.MethodPost, target: "/api/sessions", body: `{"cols":-999999999999999999999,"args":"wrong"}`, remote: "127.0.0.1:1"},
		{method: http.MethodPost, target: "/api/sessions/known/input", body: `{"data":"ok"} trailing`, remote: "127.0.0.1:1"},
		{method: http.MethodGet, target: "/api/sessions/known/events?tail=1e999&before=-99&since=not-a-number", remote: "127.0.0.1:1"},
		{method: http.MethodGet, target: "/api/fs/list?path=" + url.QueryEscape(filepath.Join(home, "..", "outside")), remote: "127.0.0.1:1"},
		{method: http.MethodGet, target: "/%2e%2e/%2e%2e/etc/passwd", remote: "198.51.100.1:9"},
		{method: http.MethodPost, target: "/api/sessions/known/upload", body: "bytes", remote: "127.0.0.1:1", filename: "../../escape?.png"},
		{method: http.MethodGet, target: "/api/sessions", remote: "198.51.100.1:9", auth: "Bearer garbage", origin: "https://evil.test"},
		{method: http.MethodGet, target: "/api/sessions?token=" + testToken, remote: "198.51.100.1:9", origin: "https://sessions.somewhere.tech"},
		{method: http.MethodGet, target: "/ws?mux=1", remote: "127.0.0.1:1", filename: "not-a-websocket-key", upgrade: true},
	}
	for _, seed := range seeds {
		f.Add(seed.method, seed.target, []byte(seed.body), seed.remote, seed.auth, seed.origin, seed.filename, seed.upgrade)
	}

	f.Fuzz(func(t *testing.T, method, target string, body []byte, remote, auth, origin, filename string, upgrade bool) {
		if !strings.HasPrefix(target, "/") {
			return
		}
		request, err := http.NewRequest(method, "http://fuzz.invalid"+target, nil)
		if err != nil {
			return
		}
		counted := &countingReader{reader: bytes.NewReader(body)}
		request.Body = io.NopCloser(counted)
		request.ContentLength = int64(len(body))
		request.RemoteAddr = remote
		request.Header.Set("Authorization", auth)
		request.Header.Set("Origin", origin)
		request.Header.Set("X-Sessions-Filename", filename)
		if upgrade {
			request.Header.Set("Connection", "keep-alive, Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", filename)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code < 100 || response.Code > 599 {
			t.Fatalf("invalid status %d", response.Code)
		}
		if counted.read > maxUploadBody+1 {
			t.Fatalf("handler read %d body bytes", counted.read)
		}
		if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
			if got != origin || !allowedOrigin(origin, config.Host) {
				t.Fatalf("unsafe ACAO %q for origin %q", got, origin)
			}
		}
		if strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") && response.Code != http.StatusNoContent {
			var decoded any
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("invalid JSON response status=%d body=%q: %v", response.Code, response.Body.String(), err)
			}
			if response.Header().Get("Vary") != "Origin" {
				t.Fatalf("JSON response missing Vary: Origin")
			}
		}
		if request.URL.Path == "/api/sessions/known/upload" && response.Code == http.StatusOK {
			var uploaded struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(response.Body.Bytes(), &uploaded) == nil && pathWithinBase(canonicalPath(uploaded.Path), canonicalPath(filepath.Join(home, ".local", "state", "sessions", "uploads"))) {
				_ = os.Remove(uploaded.Path)
			}
		}
	})
}
