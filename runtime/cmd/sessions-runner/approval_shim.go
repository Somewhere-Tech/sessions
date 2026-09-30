package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

// The permission-prompt shim is the MCP server Claude Code consults when a
// structured lane that is not fully autonomous wants to use a tool. Claude
// starts it per turn from `--mcp-config`; each tools/call is one permission
// request, forwarded to the runner over its approval endpoint and held open
// until the daemon answers. The shim keeps no state and decides nothing.

const approvalShimToolName = "approve"

// approvalPromptTool is the name Claude Code uses for the shim's tool:
// mcp__<server>__<tool>.
const approvalPromptTool = "mcp__sessions__" + approvalShimToolName

// approvalEndpoint is the runner's approval endpoint, next to its canonical
// socket so it shares the same directory and same-user guarantees.
func approvalEndpoint(socket string) string {
	return socket + "-approve"
}

// withApprovalShim adds the prompt tool and its MCP server to a Claude turn
// unless the lane bypasses permissions, in which case Claude never asks.
func withApprovalShim(args []string, executable, endpoint string) []string {
	for index, argument := range args {
		switch argument {
		case "--dangerously-skip-permissions", "--permission-prompt-tool":
			return args
		case "--permission-mode":
			if index+1 < len(args) && strings.EqualFold(args[index+1], "bypassPermissions") {
				return args
			}
		}
	}
	definition := map[string]any{
		"mcpServers": map[string]any{
			"sessions": map[string]any{
				"type": "stdio", "command": executable,
				"args": []string{"approval-shim", "--socket", endpoint},
			},
		},
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return args
	}
	return append(append([]string(nil), args...),
		"--permission-prompt-tool", approvalPromptTool, "--mcp-config", string(encoded))
}

type jsonrpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func runApprovalShim(args []string) int {
	endpoint := approvalShimEndpointArg(args)
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "approval-shim: --socket is required")
		return 2
	}
	dial := func(ctx context.Context) (net.Conn, error) { return ipc.DialContext(ctx, endpoint) }
	return serveApprovalShim(os.Stdin, os.Stdout, dial)
}

func approvalShimEndpointArg(args []string) string {
	endpoint := ""
	for index := 0; index < len(args); index++ {
		if args[index] == "--socket" && index+1 < len(args) {
			endpoint = args[index+1]
			index++
		}
	}
	return endpoint
}

func serveApprovalShim(stdin io.Reader, stdout io.Writer, dial func(context.Context) (net.Conn, error)) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writeMu sync.Mutex
	reply := func(id json.RawMessage, result any, failure *jsonrpcError) {
		if len(id) == 0 || string(id) == "null" {
			return
		}
		message := map[string]any{"jsonrpc": "2.0", "id": id}
		if failure != nil {
			message["error"] = failure
		} else {
			message["result"] = result
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return
		}
		writeMu.Lock()
		_, _ = stdout.Write(append(encoded, '\n'))
		writeMu.Unlock()
	}
	type readResult struct {
		line []byte
		err  error
	}
	reads := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(stdin)
		for {
			line, err := reader.ReadBytes('\n')
			reads <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()
	runnerGone := make(chan struct{}, 1)
	var wg sync.WaitGroup
	reading := true
	for reading {
		select {
		case result := <-reads:
			if len(strings.TrimSpace(string(result.line))) > 0 {
				var request jsonrpcRequest
				if json.Unmarshal(result.line, &request) == nil {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if handleShimRequest(ctx, request, dial, reply) {
							select {
							case runnerGone <- struct{}{}:
							default:
							}
						}
					}()
				}
			}
			if result.err != nil {
				reading = false
			}
		case <-runnerGone:
			reading = false
		}
	}
	cancel()
	if closer, ok := stdin.(io.Closer); ok {
		_ = closer.Close()
	}
	wg.Wait()
	return 0
}

// handleShimRequest reports whether the runner connection disappeared. That
// ends the per-turn shim: no later permission request could be answered by the
// runner that Claude launched it for.
func handleShimRequest(ctx context.Context, request jsonrpcRequest, dial func(context.Context) (net.Conn, error), reply func(json.RawMessage, any, *jsonrpcError)) bool {
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2024-11-05"
		}
		reply(request.ID, map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sessions", "version": version},
		}, nil)
	case "notifications/initialized", "notifications/cancelled":
	case "ping":
		reply(request.ID, map[string]any{}, nil)
	case "tools/list":
		reply(request.ID, map[string]any{"tools": []map[string]any{{
			"name":        approvalShimToolName,
			"description": "Asks the person, or the manager lane, whether Claude may use a tool. Sessions answers; the lane waits.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_name":   map[string]any{"type": "string"},
					"input":       map[string]any{"type": "object"},
					"tool_use_id": map[string]any{"type": "string"},
				},
				"required": []string{"tool_name", "input"},
			},
		}}}, nil)
	case "tools/call":
		var params struct {
			Name      string `json:"name"`
			Arguments struct {
				ToolName  string          `json:"tool_name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Name != approvalShimToolName {
			reply(request.ID, nil, &jsonrpcError{Code: -32602, Message: "unknown tool"})
			return false
		}
		decision, runnerLost := shimDecision(ctx, dial, shimApprovalRequest{
			ToolName: params.Arguments.ToolName, ToolUseID: params.Arguments.ToolUseID, Input: params.Arguments.Input,
		})
		encoded, _ := json.Marshal(decision)
		reply(request.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(encoded)}},
		}, nil)
		return runnerLost
	default:
		reply(request.ID, nil, &jsonrpcError{Code: -32601, Message: "unsupported method: " + request.Method})
	}
	return false
}

// shimDecision is the payload Claude Code reads back from the prompt tool.
// Anything but a clear allow is a deny with a reason Claude can relay.
func shimDecision(ctx context.Context, dial func(context.Context) (net.Conn, error), request shimApprovalRequest) (map[string]any, bool) {
	answer, err := askRunnerApproval(ctx, dial, request)
	if err != nil {
		return map[string]any{"behavior": "deny", "message": "Sessions could not route this permission request: " + err.Error()}, ctx.Err() == nil
	}
	switch answer.Decision {
	case proto.ApprovalAllow, proto.ApprovalAllowForSession:
		input := request.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		return map[string]any{"behavior": "allow", "updatedInput": input}, false
	default:
		by := "in Sessions"
		if answer.By != "" {
			by = "by the lane that delegated this work"
		}
		return map[string]any{"behavior": "deny", "message": "Declined " + by + ". Continue without it or explain what you need."}, false
	}
}
