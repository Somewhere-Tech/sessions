package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/coder/websocket"
)

func (a *app) cmdTail(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions tail <id> [-f] [-n N]")
	}
	idArg := args[0]
	args = args[1:]
	// Unknown and half-written options used to be skipped in silence, so
	// `tail X --lines` (no number) and `tail X --folow` both printed 50 lines
	// as if they had been understood.
	follow := false
	linesCount := 50
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-f", "--follow":
			follow = true
		case "-n", "--lines":
			if index+1 >= len(args) {
				return fail(1, "%s needs a positive integer", args[index])
			}
			value, err := strconv.Atoi(args[index+1])
			if err != nil || value < 1 {
				return fail(1, "--lines must be a positive integer")
			}
			linesCount = value
			index++
		default:
			return fail(1, "unknown tail option %s — valid options are -f/--follow and -n/--lines N", args[index])
		}
	}
	id, err := a.resolveSessionID(idArg)
	if err != nil {
		return err
	}
	text, err := a.getText("/api/sessions/" + escapeID(id) + "/snapshot")
	if err != nil {
		return err
	}
	lines := strings.Split(cleanANSI(text), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > linesCount {
		lines = lines[len(lines)-linesCount:]
	}
	if len(lines) > 0 {
		fmt.Fprintln(a.stdout, strings.Join(lines, "\n"))
	}
	if !follow {
		return nil
	}
	target, err := a.api.websocketTarget(id, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()
	connection, _, err := websocket.Dial(ctx, target.String(), nil)
	if err != nil {
		return explainAPIClientNetworkError(a.api, err)
	}
	defer connection.CloseNow()
	for {
		_, payload, err := connection.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return nil
			}
			return err
		}
		var message map[string]any
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		switch message["type"] {
		case "output":
			if data, ok := message["data"].(string); ok {
				io.WriteString(a.stdout, data)
			}
		case "exit":
			fmt.Fprintf(a.stdout, "\n[session exited code=%s]\n", nullishText(message["code"], "∅"))
			return nil
		}
	}
}

func nullishText(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
	}
	return fmt.Sprint(value)
}

func (a *app) cmdAttach(args []string) error {
	if len(args) == 0 || args[0] == "" {
		return fail(1, "usage: sessions attach <id>  (Ctrl+Q to detach)")
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	sessions, err := a.listSessions(true)
	if err != nil {
		return err
	}
	for _, current := range sessions {
		if current.ID != id {
			continue
		}
		if current.Kind == "claude-structured" {
			return fail(1, "structured Claude sessions have no live TUI attach; use `sessions snap %s` or `sessions transcript %s`", id, id)
		}
		if current.Kind != "codex-app-server" {
			continue
		}
		if current.ConversationID == "" || current.RemoteEndpoint == "" {
			return fail(2, "structured Codex session %s is missing its remote attachment metadata", id)
		}
		input, inputOK := a.stdin.(*os.File)
		output, outputOK := a.stdout.(*os.File)
		errorOutput, errorOK := a.stderr.(*os.File)
		if !inputOK || !outputOK || !errorOK || !term.IsTerminal(input.Fd()) {
			return fail(2, "attach requires an interactive terminal")
		}
		command := exec.Command("codex", "resume", "--remote", current.RemoteEndpoint, "--no-alt-screen", current.ConversationID)
		command.Dir = current.Cwd
		command.Stdin = input
		command.Stdout = output
		command.Stderr = errorOutput
		return command.Run()
	}
	input, ok := a.stdin.(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return fail(2, "attach requires an interactive terminal")
	}
	output, ok := a.stdout.(*os.File)
	if !ok {
		return fail(2, "attach requires an interactive terminal")
	}
	target, err := a.api.websocketTarget(id, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection, _, err := websocket.Dial(ctx, target.String(), nil)
	if err != nil {
		return explainAPIClientNetworkError(a.api, err)
	}
	defer connection.CloseNow()
	oldState, err := term.MakeRaw(input.Fd())
	if err != nil {
		return err
	}
	defer term.Restore(input.Fd(), oldState)

	var writes sync.Mutex
	writeMessage := func(value any) error {
		encoded, err := compactJSON(value)
		if err != nil {
			return err
		}
		writes.Lock()
		defer writes.Unlock()
		writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer writeCancel()
		return connection.Write(writeCtx, websocket.MessageText, encoded)
	}
	var resizeMu sync.Mutex
	lastColumns, lastRows := 0, 0
	resize := func() {
		columns, rows, err := term.GetSize(output.Fd())
		if err != nil || columns <= 0 || rows <= 0 {
			return
		}
		resizeMu.Lock()
		defer resizeMu.Unlock()
		if columns == lastColumns && rows == lastRows {
			return
		}
		lastColumns, lastRows = columns, rows
		_ = writeMessage(map[string]any{"type": "resize", "cols": columns, "rows": rows})
	}
	resize()
	stopResizeWatch := watchTerminalResize(ctx, resize)
	defer stopResizeWatch()
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			count, err := input.Read(buffer)
			if count > 0 {
				chunk := buffer[:count]
				if strings.ContainsRune(string(chunk), '\x11') {
					cancel()
					return
				}
				if err := writeMessage(map[string]any{"type": "input", "data": string(chunk)}); err != nil {
					cancel()
					return
				}
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		_, payload, err := connection.Read(ctx)
		if err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) != -1 {
				return nil
			}
			return err
		}
		var message map[string]any
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		switch message["type"] {
		case "output":
			if data, ok := message["data"].(string); ok {
				io.WriteString(output, data)
			}
		case "exit":
			fmt.Fprintf(output, "\n[session exited code=%s]\n", nullishText(message["code"], "∅"))
			return nil
		}
	}
}

func (a *app) cmdResize(args []string) error {
	if len(args) != 3 {
		return fail(1, "usage: sessions resize <id> <cols> <rows>")
	}
	columns, err := positiveInt(args[1], "cols")
	if err != nil {
		return err
	}
	rows, err := positiveInt(args[2], "rows")
	if err != nil {
		return err
	}
	id, err := a.resolveSessionID(args[0])
	if err != nil {
		return err
	}
	target, err := a.api.websocketTarget(id, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, target.String(), nil)
	if err != nil {
		return explainAPIClientNetworkError(a.api, err)
	}
	encoded, err := compactJSON(map[string]any{"type": "resize", "cols": columns, "rows": rows})
	if err == nil {
		err = connection.Write(ctx, websocket.MessageText, encoded)
	}
	closeErr := connection.Close(websocket.StatusNormalClosure, "resize complete")
	if err != nil {
		return err
	}
	if closeErr != nil && websocket.CloseStatus(closeErr) == -1 {
		return closeErr
	}
	if a.wantJSON {
		return writeJSON(a.stdout, struct {
			OK   bool   `json:"ok"`
			ID   string `json:"id"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}{true, id, columns, rows}, false)
	}
	_, err = fmt.Fprintf(a.stdout, "resized %s to %dx%d\n", id, columns, rows)
	return err
}
