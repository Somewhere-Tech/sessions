package claudep

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const scannerBuffer = 8 * 1024 * 1024

type Options struct {
	ClaudePath string
	Env        []string
}

type TurnOptions struct {
	CWD       string
	SessionID string
	Resume    bool
	Model     string
	ExtraArgs []string
}

type Client struct {
	claudePath string
	env        []string
}

func NewClient(options Options) (*Client, error) {
	path := strings.TrimSpace(options.ClaudePath)
	if path == "" {
		var err error
		path, err = exec.LookPath("claude")
		if err != nil {
			return nil, fmt.Errorf("find claude: %w", err)
		}
	}
	env := options.Env
	if env == nil {
		env = os.Environ()
	}
	return &Client{claudePath: path, env: withoutAnthropicAPIKey(env)}, nil
}

// SendTurn starts one independent claude -p process. Resume must be false for
// the first turn and true for all later turns sharing SessionID.
func (c *Client) SendTurn(ctx context.Context, prompt string, options TurnOptions) (*TurnStream, error) {
	if strings.TrimSpace(options.SessionID) == "" {
		return nil, errors.New("Claude session id is required")
	}
	if prompt == "" {
		return nil, errors.New("Claude prompt is required")
	}
	args := turnArgs(prompt, options)
	command := exec.CommandContext(ctx, c.claudePath, args...)
	command.Dir = options.CWD
	command.Env = append([]string(nil), c.env...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start claude -p turn: %w", err)
	}
	events := make(chan Event, 128)
	state := newTurnState()
	stream := &TurnStream{Events: events, state: state}
	go consumeTurn(command, stdout, &stderr, options.SessionID, events, state)
	return stream, nil
}

func consumeTurn(command *exec.Cmd, stdout io.Reader, stderr *bytes.Buffer, expectedSessionID string, events chan<- Event, state *turnState) {
	defer close(events)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), scannerBuffer)
	result := TurnResult{SessionID: expectedSessionID}
	var streamErr error
	sawResult := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		event, err := parseStreamJSONLine(append(json.RawMessage(nil), line...), time.Now(), expectedSessionID)
		if err != nil {
			streamErr = errors.Join(streamErr, err)
			continue
		}
		events <- event
		if event.Type != "result" {
			continue
		}
		sawResult = true
		result.Message = event.Message
		result.Usage = append(json.RawMessage(nil), event.Usage...)
		result.Subtype = event.Subtype
		var value struct {
			IsError bool `json:"is_error"`
		}
		_ = json.Unmarshal(event.Raw, &value)
		result.IsError = value.IsError
	}
	if err := scanner.Err(); err != nil {
		streamErr = errors.Join(streamErr, fmt.Errorf("read Claude stream-json: %w", err))
	}
	waitErr := command.Wait()
	stderrText := strings.TrimSpace(stderr.String())
	if waitErr != nil {
		if stderrText != "" {
			waitErr = fmt.Errorf("claude -p turn: %w: %s", waitErr, stderrText)
		} else {
			waitErr = fmt.Errorf("claude -p turn: %w", waitErr)
		}
		streamErr = errors.Join(streamErr, waitErr)
	}
	if !sawResult {
		missing := errors.New("Claude stream-json ended without a result event")
		if stderrText != "" && waitErr == nil {
			missing = fmt.Errorf("%w: %s", missing, stderrText)
		}
		streamErr = errors.Join(streamErr, missing)
	}
	if result.IsError && streamErr == nil {
		streamErr = fmt.Errorf("Claude turn result reported %s", result.Subtype)
	}
	state.finish(result, streamErr)
}

func turnArgs(prompt string, options TurnOptions) []string {
	args := sanitizeExtraArgs(options.ExtraArgs)
	args = append(args, "-p", "--output-format", "stream-json", "--verbose")
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	if options.Resume {
		args = append(args, "--resume", options.SessionID)
	} else {
		args = append(args, "--session-id", options.SessionID)
	}
	return append(args, prompt)
}

func sanitizeExtraArgs(args []string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		skipValue := false
		switch argument {
		case "-p", "--print", "--verbose":
			continue
		case "--session-id", "--resume", "-r", "--output-format", "--input-format", "--model", "-m":
			skipValue = true
		}
		if strings.HasPrefix(argument, "--session-id=") || strings.HasPrefix(argument, "--resume=") ||
			strings.HasPrefix(argument, "--output-format=") || strings.HasPrefix(argument, "--input-format=") ||
			strings.HasPrefix(argument, "--model=") {
			continue
		}
		if skipValue {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		result = append(result, argument)
	}
	return result
}

func withoutAnthropicAPIKey(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key == "ANTHROPIC_API_KEY" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func NewSessionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
