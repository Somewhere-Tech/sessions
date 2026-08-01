package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
)

type cliFailure struct {
	message string
	code    int
	quiet   bool
}

func (e *cliFailure) Error() string { return e.message }

func fail(code int, format string, args ...any) error {
	return &cliFailure{message: fmt.Sprintf(format, args...), code: code}
}

func status(code int) error { return &cliFailure{code: code, quiet: true} }

func exitCode(err error) int {
	var failure *cliFailure
	if errors.As(err, &failure) {
		if failure.code > 0 {
			return failure.code
		}
		return 1
	}
	return 2
}

func writeFailure(stderr io.Writer, err error) {
	var failure *cliFailure
	if errors.As(err, &failure) {
		if failure.quiet || failure.message == "" {
			return
		}
		fmt.Fprintf(stderr, "sessions: %s\n", failure.message)
		return
	}
	fmt.Fprintf(stderr, "sessions: %s\n", err)
}

type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	args           []string
	sub            string
	host           string
	port           string
	wantJSON       bool
	exitCode       int
	home           string
	now            func() time.Time
	sleep          func(time.Duration)
	api            *apiClient
	explicitTarget bool
	listModels     func(context.Context) ([]codexapp.Model, error)
	runUpdate      func(context.Context, bool) (nativeUpdateResult, error)
	cliIsCurrent   func(string) bool
	attachSupport  func(context.Context, supportAttachmentRequest) (supportAttachmentReceipt, error)
	commands       []commandSpec
}

func newApp(arguments []string, stdin io.Reader, stdout, stderr io.Writer) (*app, error) {
	args, host, port, wantJSON := parseGlobalArgs(arguments)
	explicitTarget := hasExplicitConnectionTarget(arguments)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	app := &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		args: args, host: host, port: port, wantJSON: wantJSON, explicitTarget: explicitTarget,
		home: home, now: time.Now, sleep: time.Sleep, listModels: listLiveCodexModels,
		runUpdate:    runNativeAppUpdate,
		cliIsCurrent: installedCLIMatches,
		commands:     commandTable,
	}
	app.attachSupport = app.attachSupportWithSomewhere
	tokenPath := ""
	localToken := cliHostIsLoopback(host)
	if localToken {
		tokenPath, err = sessionstate.LocalTokenPathFromEnv()
		if err != nil {
			return nil, fmt.Errorf("resolve local daemon token path: %w", err)
		}
	}
	if machineName, selected := strings.CutPrefix(host, "machine:"); selected {
		machine, err := loadSavedMachine(home, machineName)
		if err != nil {
			return nil, err
		}
		host = machine.Endpoint
		port = ""
		tokenPath = savedMachineTokenPath(home, machine.MachineID)
		localToken = false
		app.host = host
		app.port = port
	}
	if len(app.args) > 0 {
		app.sub = app.args[0]
		app.args = app.args[1:]
	}
	client, err := newAPIClient(host, port, tokenPath, localToken)
	if err != nil {
		return nil, fail(2, "%s", err)
	}
	app.api = client
	return app, nil
}

func hasExplicitConnectionTarget(arguments []string) bool {
	if _, set := os.LookupEnv("SESSIONS_HOST"); set {
		return true
	}
	if _, set := os.LookupEnv("SESSIONS_PORT"); set {
		return true
	}
	for _, argument := range arguments {
		if argument == "--" {
			return false
		}
		switch argument {
		case "--host", "--port", "--machine":
			return true
		case "--json":
			continue
		default:
			return false
		}
	}
	return false
}

func (a *app) close() {
	if a.api != nil {
		a.api.close()
	}
}

func (a *app) dispatch() error {
	if a.sub == "" {
		return a.cmdSessions(a.args)
	}
	if a.sub == "--help" || a.sub == "-h" {
		return writeTopLevelHelp(a.stdout)
	}
	if a.sub == "help" {
		if len(a.args) == 0 || a.args[0] == "--help" || a.args[0] == "-h" {
			return writeTopLevelHelp(a.stdout)
		}
		return writeCommandHelp(a.stdout, a.args[0])
	}
	command, ok := lookupCommand(a.sub)
	if !ok {
		if helpRequested(a.args) {
			return writeTopLevelHelp(a.stdout)
		}
		return fail(1, "unknown command: %s\n\nrun 'sessions help' for usage", a.sub)
	}
	if helpRequested(a.args) {
		return writeCommandHelp(a.stdout, command.name)
	}
	args := append([]string(nil), a.args...)
	if command.localJSON && removeBeforeSeparator(&args, "--json") {
		a.wantJSON = true
	}
	return command.run(a, args)
}

// parseGlobalArgs deliberately considers only the prefix before the command.
// Once the command token (or a bare --) is observed, the remaining arguments
// are opaque to global parsing. In particular, a run child receives every
// argument following its -- separator unchanged.
func parseGlobalArgs(arguments []string) (args []string, host, port string, wantJSON bool) {
	host = getenv("SESSIONS_HOST", "127.0.0.1")
	port = getenv("SESSIONS_PORT", "8787")
	index := 0
	for index < len(arguments) {
		switch arguments[index] {
		case "--json":
			wantJSON = true
			index++
		case "--host", "--port", "--machine":
			name := arguments[index]
			if index+1 >= len(arguments) || arguments[index+1] == "--" {
				return append([]string(nil), arguments[index:]...), host, port, wantJSON
			}
			if name == "--host" {
				host = arguments[index+1]
			} else if name == "--machine" {
				host = "machine:" + arguments[index+1]
				port = ""
			} else {
				port = arguments[index+1]
			}
			index += 2
		default:
			return append([]string(nil), arguments[index:]...), host, port, wantJSON
		}
	}
	return nil, host, port, wantJSON
}

func removeFirst(args *[]string, target string) bool {
	for i, arg := range *args {
		if arg == target {
			*args = append((*args)[:i], (*args)[i+1:]...)
			return true
		}
	}
	return false
}

func removeBeforeSeparator(args *[]string, target string) bool {
	for index, argument := range *args {
		if argument == "--" {
			return false
		}
		if argument == target {
			*args = append((*args)[:index], (*args)[index+1:]...)
			return true
		}
	}
	return false
}

func pluck(args *[]string, target string) (string, bool) {
	for i, arg := range *args {
		if arg != target {
			continue
		}
		if i+1 >= len(*args) {
			*args = (*args)[:i]
			return "", true
		}
		value := (*args)[i+1]
		*args = append((*args)[:i], (*args)[i+2:]...)
		return value, true
	}
	return "", false
}

func (a *app) configureCreateOwner(args *[]string) error {
	owner, explicit := pluck(args, "--owner")
	detach := removeFirst(args, "--detach")
	if !explicit {
		if detach {
			return fail(1, "--detach requires --owner")
		}
		return nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fail(1, "--owner needs a non-empty id")
	}
	if a.api.creatorSession != "" && !detach {
		return fail(1, "--owner conflicts with inherited SESSIONS_SESSION_ID; pass --detach to create an external root")
	}
	a.api.ownerID = owner
	return nil
}

func writeJSON(w io.Writer, value any, indent bool) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func compactJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

var durationPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(ms|s|m|h)?$`)

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	match := durationPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fail(1, "bad duration '%s' — try 2s, 500ms, 1m, 30s", raw)
	}
	number, _ := strconv.ParseFloat(match[1], 64)
	multiplier := time.Second
	switch strings.ToLower(match[2]) {
	case "ms":
		multiplier = time.Millisecond
	case "m":
		multiplier = time.Minute
	case "h":
		multiplier = time.Hour
	}
	return time.Duration(number * float64(multiplier)), nil
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
