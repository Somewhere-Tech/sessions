package waitcond

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrettyWaitCLIEndToEnd(t *testing.T) {
	binary := buildPrettyCLI(t)
	home := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "runners")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "HOME="+home, "SESSIONS_STATE_DIR="+stateDir)

	t.Run("commit metadata fallback timeout and force reset", func(t *testing.T) {
		repo := newGitRepo(t)
		id := "commit-fallback-session"
		writeRunnerMetadata(t, stateDir, id, repo)
		initial := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

		code, stdout, stderr := runPrettyCLIOnBaseline(t, binary, environment, func() {
			writeFile(t, filepath.Join(repo, "work.txt"), "next\n")
			git(t, repo, "add", "work.txt")
			git(t, repo, "commit", "-q", "-m", "CLI real commit")
		},
			"--port", "1", "--json", "wait", id, "--until", "commit", "--timeout", "3s")
		if code != 0 {
			t.Fatalf("exit = %d, stdout=%s stderr=%s", code, stdout, stderr)
		}
		var commitOutput struct {
			Session          string `json:"session"`
			Subject          string `json:"subject"`
			Baseline         string `json:"baseline"`
			Commit           string `json:"commit"`
			HistoryRewritten bool   `json:"history_rewritten"`
		}
		if err := json.Unmarshal(stdout, &commitOutput); err != nil {
			t.Fatal(err)
		}
		if commitOutput.Session != id || commitOutput.Subject != "CLI real commit" || commitOutput.Baseline != initial || commitOutput.Commit == commitOutput.Baseline || commitOutput.HistoryRewritten {
			t.Fatalf("unexpected output: %#v", commitOutput)
		}
		t.Logf("commit JSON: %s", bytes.TrimSpace(stdout))

		code, stdout, stderr = runPrettyCLI(t, binary, environment,
			"--port", "1", "--json", "wait", id, "--until", "commit", "--timeout", "120ms")
		if code != 2 {
			t.Fatalf("exit = %d, want 2; stdout=%s stderr=%s", code, stdout, stderr)
		}
		var timeoutOutput struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(stdout, &timeoutOutput); err != nil || timeoutOutput.Reason != "timeout" {
			t.Fatalf("timeout output = %q, decode err = %v", stdout, err)
		}
		t.Logf("timeout exit=%d JSON: %s", code, bytes.TrimSpace(stdout))

		beforeReset := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
		code, stdout, stderr = runPrettyCLIOnBaseline(t, binary, environment, func() {
			git(t, repo, "reset", "--hard", "HEAD^")
		},
			"--port", "1", "--json", "wait", id, "--until", "commit", "--timeout", "3s")
		if code != 0 {
			t.Fatalf("exit = %d, stdout=%s stderr=%s", code, stdout, stderr)
		}
		var resetOutput struct {
			HistoryRewritten bool   `json:"history_rewritten"`
			Subject          string `json:"subject"`
			Baseline         string `json:"baseline"`
		}
		if err := json.Unmarshal(stdout, &resetOutput); err != nil {
			t.Fatal(err)
		}
		if !resetOutput.HistoryRewritten || resetOutput.Subject != "initial" || resetOutput.Baseline != beforeReset {
			t.Fatalf("unexpected force-reset output: %#v", resetOutput)
		}
		t.Logf("force-reset JSON: %s", bytes.TrimSpace(stdout))
	})

	t.Run("any returns second session", func(t *testing.T) {
		root := t.TempDir()
		writeRunnerMetadata(t, stateDir, "first-session", root)
		writeRunnerMetadata(t, stateDir, "second-session", root)
		writeFile(t, filepath.Join(root, "second.log"), "SECOND WON\n")
		code, stdout, stderr := runPrettyCLI(t, binary, environment,
			"--port", "1", "--json", "wait", "first-session", "second-session", "--any",
			"--until-file-contains", "first.log", "FIRST WON",
			"--until-file-contains", "second.log", "SECOND WON",
			"--timeout", "2s")
		if code != 0 {
			t.Fatalf("exit = %d, stdout=%s stderr=%s", code, stdout, stderr)
		}
		var output struct {
			Session string `json:"session"`
			File    string `json:"file"`
		}
		if err := json.Unmarshal(stdout, &output); err != nil {
			t.Fatal(err)
		}
		if output.Session != "second-session" || filepath.Base(output.File) != "second.log" {
			t.Fatalf("unexpected --any winner: %#v", output)
		}
		t.Logf("--any JSON: %s", bytes.TrimSpace(stdout))
	})

	t.Run("idle stable labels structured evidence", func(t *testing.T) {
		root := t.TempDir()
		id := "idle-session"
		daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/sessions":
				_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []map[string]any{{
					"id": id, "cwd": root, "cmd": "codex", "tool": "codex", "working": false,
				}}})
			case "/api/sessions/" + id + "/wait":
				_ = json.NewEncoder(response).Encode(map[string]any{
					"session": id, "cwd": root, "working": false, "source": "structured",
				})
			default:
				http.NotFound(response, request)
			}
		}))
		defer daemon.Close()
		code, stdout, stderr := runPrettyCLI(t, binary, environment,
			"--host", daemon.URL, "--json", "wait", id,
			"--until-idle-stable", "80ms", "--timeout", "1s")
		if code != 0 {
			t.Fatalf("exit = %d, stdout=%s stderr=%s", code, stdout, stderr)
		}
		var output struct {
			Source       string `json:"source"`
			IdleStableMS int64  `json:"idle_stable_ms"`
		}
		if err := json.Unmarshal(stdout, &output); err != nil {
			t.Fatal(err)
		}
		if output.Source != "structured" || output.IdleStableMS != 80 {
			t.Fatalf("unexpected idle result: %#v", output)
		}
		t.Logf("idle-stable JSON: %s", bytes.TrimSpace(stdout))
	})
}

func buildPrettyCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "sessions")
	command := exec.Command("go", "build", "-o", binary, "../../cmd/sessions")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build sessions CLI: %v\n%s", err, output)
	}
	return binary
}

func runPrettyCLI(t *testing.T, binary string, environment []string, args ...string) (int, []byte, []byte) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.Bytes(), stderr.Bytes()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.Bytes(), stderr.Bytes()
	}
	t.Fatalf("run sessions: %v", err)
	return -1, nil, nil
}

// runPrettyCLIOnBaseline waits for the CLI's first successful
// `git rev-parse --verify HEAD` to finish before mutating the repository. The
// wrapper makes baseline capture observable without adding a production test
// hook; the mutation may then race ahead of fsnotify registration, which also
// exercises the required subscribe-then-recheck ordering end to end.
func runPrettyCLIOnBaseline(t *testing.T, binary string, environment []string, mutate func(), args ...string) (int, []byte, []byte) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	marker := filepath.Join(wrapperDir, "baseline-ready")
	wrapper := filepath.Join(wrapperDir, "git")
	script := `#!/bin/sh
output=$("$SESSIONS_WAIT_REAL_GIT" "$@")
status=$?
if [ "$status" -eq 0 ] && [ "$#" -ge 5 ] && [ "$1" = "-C" ] && [ "$3" = "rev-parse" ] && [ "$4" = "--verify" ] && [ "$5" = "HEAD" ]; then
  : > "$SESSIONS_WAIT_BASELINE_READY"
fi
if [ -n "$output" ]; then
  printf '%s\n' "$output"
fi
exit "$status"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	environment = setTestEnv(environment, "PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	environment = setTestEnv(environment, "SESSIONS_WAIT_REAL_GIT", realGit)
	environment = setTestEnv(environment, "SESSIONS_WAIT_BASELINE_READY", marker)
	wake, closeWake := watchParent(marker)
	defer closeWake()

	command := exec.Command(binary, args...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = command.Process.Kill()
			<-done
		}
	})
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = command.Process.Kill()
			<-done
			waited = true
			t.Fatalf("inspect baseline marker: %v", err)
		}
		select {
		case <-wake:
		case err := <-done:
			waited = true
			t.Fatalf("CLI exited before capturing its Git baseline: err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
		}
	}

	mutate()
	err = <-done
	waited = true
	if err == nil {
		return 0, stdout.Bytes(), stderr.Bytes()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.Bytes(), stderr.Bytes()
	}
	t.Fatalf("run sessions: %v", err)
	return -1, nil, nil
}

func setTestEnv(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}

func writeRunnerMetadata(t *testing.T, stateDir, id, cwd string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"id": id, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, id+".json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
