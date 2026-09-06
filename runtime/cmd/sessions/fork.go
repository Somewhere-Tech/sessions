package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
)

func (a *app) cmdFork(args []string) error {
	destinationProvider, destinationSet := pluck(&args, "--with")
	pointValue, pointSet := pluck(&args, "--at")
	messageID, messageIDSet := pluck(&args, "--message-id")
	briefingFile, briefingSet := pluck(&args, "--briefing-file")
	generate := removeFirst(&args, "--generate-briefing")
	profile, profileSet := pluck(&args, "--profile")
	name, _ := pluck(&args, "--name")
	model, _ := pluck(&args, "--model")
	effort, _ := pluck(&args, "--effort")
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions fork <session> [--with claude|codex] [--at MESSAGE_INDEX [--message-id ID]]")
	}
	if destinationSet && destinationProvider != "claude" && destinationProvider != "codex" {
		return fail(1, "--with must be claude or codex")
	}
	sourceID, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	body := map[string]any{"sourceSessionId": sourceID}
	if destinationSet {
		body["destinationProvider"] = destinationProvider
	}
	if messageIDSet && !pointSet {
		return fail(1, "--message-id requires --at")
	}
	if pointSet {
		pointIndex, err := strconv.Atoi(strings.TrimSpace(pointValue))
		if err != nil || pointIndex < 0 {
			return fail(1, "--at must be a non-negative message index")
		}
		body["sourceMessageIndex"] = pointIndex
		if messageIDSet {
			body["sourceMessageId"] = strings.TrimSpace(messageID)
		}
	}
	var result recovery.AdoptResult
	path, err := a.prepareForkContext(body, briefingFile, briefingSet, generate, profile, profileSet)
	if err != nil {
		return err
	}
	if generate {
		return nil
	}
	body["name"], body["model"], body["effort"] = name, model, effort
	if err := a.postJSON(path, body, &result, 2); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if _, err := fmt.Fprintln(a.stdout, result.LaneID); err != nil {
		return err
	}
	if briefingSet {
		fmt.Fprintln(a.stderr, "sessions: started a collaborator with the reviewed briefing; the source conversation is unchanged")
		return nil
	}
	fmt.Fprintf(
		a.stderr,
		"sessions: copied %d authored messages into %s; source %s keeps running\n",
		result.ImportedMessages, result.DestinationProvider, result.ForkedFromSessionID,
	)
	return nil
}

func (a *app) prepareForkContext(body map[string]any, file string, briefingSet, generate bool, profile string, profileSet bool) (string, error) {
	if generate {
		if briefingSet || profileSet {
			return "", fail(1, "--generate-briefing uses the source account and cannot be combined with --briefing-file or --profile")
		}
		var result struct {
			Briefing        string `json:"briefing"`
			SourceUntouched bool   `json:"sourceUntouched"`
		}
		if err := a.postJSON("/api/recovery/briefing", body, &result, 2); err != nil {
			return "", err
		}
		if a.wantJSON {
			return "", writeJSON(a.stdout, result, true)
		}
		_, err := fmt.Fprintln(a.stdout, result.Briefing)
		return "", err
	}
	path := "/api/recovery/fork"
	if briefingSet || profileSet {
		path = "/api/recovery/collaborator"
		body["contextMode"] = "conversation"
	}
	if profileSet {
		body["profile"] = profile
	}
	if briefingSet {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(io.LimitReader(a.stdin, 24*1024+1))
		} else {
			var input *os.File
			input, err = os.Open(file)
			if err == nil {
				defer input.Close()
				data, err = io.ReadAll(io.LimitReader(input, 24*1024+1))
			}
		}
		if err != nil {
			return "", fail(1, "read briefing: %v", err)
		}
		if len(data) > 24*1024 || strings.TrimSpace(string(data)) == "" {
			return "", fail(1, "briefing must contain 1 to 24576 bytes")
		}
		body["contextMode"], body["briefing"] = "briefing", string(data)
	}
	return path, nil
}
