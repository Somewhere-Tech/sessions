package main

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

func (a *app) cmdSearch(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return searchUsage()
	}
	queryText := args[0]
	args = args[1:]
	regex := removeFirst(&args, "--regex")
	exact := removeFirst(&args, "--exact")
	rankedFlag := removeFirst(&args, "--ranked")
	if (rankedFlag && regex) || (exact && regex) || (exact && rankedFlag) {
		return fail(1, "--exact, --regex, and --ranked cannot be combined")
	}
	ranked := !exact && !regex
	sessionID, hasSession := pluck(&args, "--session")
	role, hasRole := pluck(&args, "--role")
	tool, hasTool := pluck(&args, "--tool")
	name, hasName := pluck(&args, "--name")
	lane, hasLane := pluck(&args, "--lane")
	cwd, hasCWD := pluck(&args, "--cwd")
	since, hasSince := pluck(&args, "--since")
	until, hasUntil := pluck(&args, "--until")
	contextText, hasContext := pluck(&args, "--context")
	timeline := removeFirst(&args, "--timeline")
	limitText, hasLimit := pluck(&args, "-n")
	if len(args) != 0 {
		if strings.HasPrefix(queryText, "-") {
			return fail(1, "the query must come before flags, but %q looks like a flag used as the query.\ntry: sessions search %q %s ...\n%s", queryText, args[0], queryText, searchUsageText)
		}
		return fail(1, "unknown search option: %s\n%s", args[0], searchUsageText)
	}
	if hasSession && strings.TrimSpace(sessionID) == "" {
		return fail(1, "--session needs a session id")
	}
	role = strings.ToLower(role)
	if hasRole && role != "user" && role != "assistant" && role != "tool" {
		return fail(1, "--role must be \"user\", \"assistant\", or \"tool\"")
	}
	tool = strings.ToLower(tool)
	if hasTool && tool != "claude" && tool != "codex" && tool != "shell" {
		return fail(1, "--tool must be \"claude\", \"codex\", or \"shell\"")
	}
	limit := 0
	if hasLimit {
		parsed, err := strconv.Atoi(limitText)
		if err != nil || parsed < 1 || parsed > historysearch.MaxLimit {
			return fail(1, "-n must be between 1 and %d", historysearch.MaxLimit)
		}
		limit = parsed
	}
	contextCount := 0
	if hasContext {
		parsed, err := strconv.Atoi(contextText)
		if err != nil || parsed < 0 || parsed > historysearch.MaxContext {
			return fail(1, "--context must be between 0 and %d", historysearch.MaxContext)
		}
		contextCount = parsed
	}
	if hasName && hasLane {
		return fail(1, "--name and --lane are aliases; use only one")
	}

	parameters := url.Values{"q": {queryText}}
	if hasSession {
		parameters.Set("session", sessionID)
	}
	if hasRole {
		parameters.Set("role", role)
	}
	if hasTool {
		parameters.Set("tool", tool)
	}
	if hasName {
		parameters.Set("name", name)
	}
	if hasLane {
		parameters.Set("name", lane)
	}
	if hasCWD {
		parameters.Set("cwd", cwd)
	}
	if hasSince {
		parameters.Set("since", since)
	}
	if hasUntil {
		parameters.Set("until", until)
	}
	if hasContext {
		parameters.Set("context", strconv.Itoa(contextCount))
	}
	if timeline {
		parameters.Set("timeline", "true")
	}
	if regex {
		parameters.Set("regex", "true")
	}
	if ranked {
		parameters.Set("ranked", "true")
	}
	if hasLimit {
		parameters.Set("limit", strconv.Itoa(limit))
	}
	var result historysearch.Response
	if err := a.getJSON("/api/search?"+parameters.Encode(), &result); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	if len(result.Matches) == 0 {
		_, err := io.WriteString(a.stdout, "(no matches)\n")
		return err
	}
	for _, match := range result.Matches {
		name := match.Name
		if name == "" {
			name = "(unnamed)"
		}
		timestamp := "(no timestamp)"
		if match.Timestamp != nil && *match.Timestamp != "" {
			timestamp = *match.Timestamp
		}
		score := ""
		if ranked {
			score = "  " + rankedMatchLabel(match.Score)
		}
		displayRole := match.Role
		if match.Role == "tool" && match.Kind != "" {
			displayRole = match.Kind
		}
		fmt.Fprintf(a.stdout, "%s  %s  %s%s\n  %s  %s  message %d\n",
			prefixString(match.SessionID, 8), name, match.Tool, score,
			displayRole, timestamp, match.MessageIndex+1)
		for _, message := range match.ContextBefore {
			fmt.Fprintf(a.stdout, "    before · %s: %s\n", message.Role, compactSearchText(message.Text))
		}
		fmt.Fprintf(a.stdout, "    %s\n", match.Snippet)
		for _, message := range match.ContextAfter {
			fmt.Fprintf(a.stdout, "    after · %s: %s\n", message.Role, compactSearchText(message.Text))
		}
		fmt.Fprintln(a.stdout)
	}
	return nil
}

func rankedMatchLabel(score float64) string {
	switch {
	case score >= 0.85:
		return "best match"
	case score >= 0.5:
		return "strong match"
	default:
		return "related"
	}
}

func compactSearchText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		return value[:239] + "…"
	}
	return value
}

const searchUsageText = "usage: sessions search <query> [--session ID[,ID...]] [--role user|assistant|tool] [--tool claude|codex|shell] [--name GLOB] [--cwd PATH] [--since DATE] [--until DATE] [--context N] [--timeline] [-n N] [--exact | --regex | --ranked] [--json]\nranked token search is the default; use --exact for a contiguous literal phrase. The query comes FIRST, before any flags"

func searchUsage() error {
	return fail(1, searchUsageText)
}
