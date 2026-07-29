package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRunDoubleDashPreservationMatrix(t *testing.T) {
	const laneID = "00000000-0000-4000-8000-000000000099"
	tests := []struct {
		name  string
		child []string
	}{
		{name: "json", child: []string{"tool", "--json"}},
		{name: "host and value", child: []string{"tool", "--host", "elsewhere.example"}},
		{name: "port and value", child: []string{"tool", "--port", "9999"}},
		{name: "all reserved globals", child: []string{"tool", "--json", "--host", "elsewhere.example", "--port", "9999"}},
		{name: "shell argv0 reproduction", child: []string{"bash", "-c", `printf "argv0=%s\n" "$0"`, "--json"}},
		{name: "second separator and help", child: []string{"tool", "--", "--help", "-h", "--json"}},
		{name: "empty and exact bytes", child: []string{"tool", "", "two words", "line\nbreak", "--host=literal"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var posted runLaneRequest
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/lanes" {
					http.NotFound(response, request)
					return
				}
				if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
					t.Errorf("decode request: %v", err)
				}
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(response, `{"id":"`+laneID+`"}`)
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())

			arguments := []string{"--host", server.URL, "run", "--"}
			arguments = append(arguments, test.child...)
			var stdout, stderr bytes.Buffer
			code := run(arguments, strings.NewReader(""), &stdout, &stderr)
			if code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.String() != laneID+"\n" {
				t.Fatalf("stdout = %q, want default id output", stdout.String())
			}
			got := append([]string{posted.Cmd}, posted.Args...)
			if !reflect.DeepEqual(got, test.child) {
				t.Fatalf("child argv changed\n got: %#v\nwant: %#v", got, test.child)
			}
		})
	}
}

func TestGlobalParsingStopsAtCommandAndFirstSeparator(t *testing.T) {
	t.Setenv("SESSIONS_HOST", "env-host")
	t.Setenv("SESSIONS_PORT", "7000")
	tests := []struct {
		name     string
		input    []string
		wantArgs []string
		wantHost string
		wantPort string
		wantJSON bool
	}{
		{
			name: "prefix globals", input: []string{"--json", "--host", "prefix-host", "--port", "8000", "run", "--", "tool", "--json"},
			wantArgs: []string{"run", "--", "tool", "--json"}, wantHost: "prefix-host", wantPort: "8000", wantJSON: true,
		},
		{
			name: "nothing after subcommand", input: []string{"run", "--json", "--host", "child-host", "--", "tool", "--port", "9000"},
			wantArgs: []string{"run", "--json", "--host", "child-host", "--", "tool", "--port", "9000"}, wantHost: "env-host", wantPort: "7000",
		},
		{
			name: "nothing after first separator", input: []string{"--", "--json", "--host", "child-host"},
			wantArgs: []string{"--", "--json", "--host", "child-host"}, wantHost: "env-host", wantPort: "7000",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args, host, port, wantJSON := parseGlobalArgs(test.input)
			if !reflect.DeepEqual(args, test.wantArgs) || host != test.wantHost || port != test.wantPort || wantJSON != test.wantJSON {
				t.Fatalf("parseGlobalArgs() = args=%#v host=%q port=%q json=%v", args, host, port, wantJSON)
			}
		})
	}
}

func TestDeclarativeHelpIsCompleteDailyFirstAndSuccessful(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("top-level help exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	top := stdout.String()
	for _, required := range []string{"run", "recover", "move", "adopt", "backup", "models", "docs", "Admin/operational:"} {
		if !strings.Contains(top, required) {
			t.Errorf("top-level help missing %q", required)
		}
	}
	previous := -1
	for _, name := range []string{"new", "run", "ls", "list", "lanes", "send", "ask", "wait", "last", "status", "kill", "recover"} {
		index := strings.Index(top, fmt.Sprintf("\n  %-24s", name))
		if index < 0 {
			t.Fatalf("top-level help missing daily command %q", name)
		}
		if index <= previous {
			t.Fatalf("daily command %q is out of order", name)
		}
		previous = index
	}

	for _, command := range commandTable {
		if command.name == "" || command.usage == "" || command.summary == "" || command.longHelp == "" || len(command.examples) == 0 || command.group == "" || command.run == nil {
			t.Errorf("incomplete command table entry: %#v", command)
			continue
		}
		if !strings.Contains(top, "\n  "+command.name) {
			t.Errorf("top-level help missing command %q", command.name)
		}
		if command.name == "help" {
			continue
		}
		stdout.Reset()
		stderr.Reset()
		code := run([]string{command.name, "--help"}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 {
			t.Errorf("%s --help exit=%d stdout=%q stderr=%q", command.name, code, stdout.String(), stderr.String())
			continue
		}
		if !strings.Contains(stdout.String(), "Usage:\n  sessions "+command.name) || !strings.Contains(stdout.String(), "Examples:") {
			t.Errorf("%s --help is not detailed:\n%s", command.name, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"help", "run"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("help run exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := stdout.String()
	stdout.Reset()
	if code := run([]string{"run", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.String() != want {
		t.Fatalf("run --help does not match help run: exit=%d\n%s", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"not-a-command", "--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "Daily workflows:") {
		t.Fatalf("unknown --help exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"docs"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("docs exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	fullDocs := stdout.String()
	if !strings.HasPrefix(fullDocs, "<!-- GENERATED by `sessions docs`") || !strings.Contains(fullDocs, "# Sessions CLI reference") {
		t.Fatalf("docs output is not the generated Markdown reference:\n%s", fullDocs)
	}
	if count := strings.Count(fullDocs, "\n## `sessions "); count != len(commandTable) {
		t.Fatalf("docs command sections = %d, want %d", count, len(commandTable))
	}
	for _, command := range commandTable {
		if !strings.Contains(fullDocs, "\n## `sessions "+command.name+"`\n") {
			t.Errorf("docs missing command %q", command.name)
		}
	}
}

func TestDeployIsRetiredAndNonMutating(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"deploy"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("deploy exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "retired with the Node daemon") || !strings.Contains(stderr.String(), "no changes were made") {
		t.Fatalf("deploy stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunWaitPropagatesExitAndOptionallyPrintsOutput(t *testing.T) {
	const laneID = "00000000-0000-4000-8000-000000000055"
	tests := []struct {
		name       string
		exitCode   int
		output     bool
		wantStdout string
	}{
		{name: "success summary", exitCode: 0, wantStdout: laneID + " exited 0 after 125ms\n"},
		{name: "failure output tail", exitCode: 7, output: true, wantStdout: "captured tail\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/api/lanes":
					response.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(response, `{"id":"`+laneID+`"}`)
				case request.Method == http.MethodGet && request.URL.Path == "/api/lanes/"+laneID+"/manifest":
					manifestRequests++
					_, _ = fmt.Fprintf(response, `{"exit_code":%d,"duration_ms":125,"last_output_tail":"captured tail","spec_path":""}`, test.exitCode)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())

			arguments := []string{"--host", server.URL, "run", "--wait"}
			if test.output {
				arguments = append(arguments, "--output")
			}
			arguments = append(arguments, "--", "sh", "-c", "exit 7")
			var stdout, stderr bytes.Buffer
			code := run(arguments, strings.NewReader(""), &stdout, &stderr)
			if code != test.exitCode || stderr.Len() != 0 || stdout.String() != test.wantStdout {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if manifestRequests < 2 {
				t.Fatalf("manifest requests = %d, want wait observation plus final fetch", manifestRequests)
			}
		})
	}
}

func TestRunWithoutWaitKeepsIDAndJSONShapes(t *testing.T) {
	const responseBody = `{"id":"00000000-0000-4000-8000-000000000044","kind":"lane","name":"shape"}`
	manifestRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost && request.URL.Path == "/api/lanes" {
			response.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(response, responseBody)
			return
		}
		manifestRequests++
		http.NotFound(response, request)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "run", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != "00000000-0000-4000-8000-000000000044\n" || stderr.Len() != 0 {
		t.Fatalf("plain exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = run([]string{"--host", server.URL, "--json", "run", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var actual, expected any
	if err := json.Unmarshal(stdout.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(responseBody), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("run JSON changed: got %#v want %#v", actual, expected)
	}
	if manifestRequests != 0 {
		t.Fatalf("non-wait run made %d extra requests", manifestRequests)
	}
}

func TestRunOutputRequiresWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--output", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "--output requires --wait") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
