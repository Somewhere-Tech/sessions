package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func stateFor(t *testing.T) state.Paths { t.Helper(); return state.For(t.TempDir(), "claude-session") }

func osCreate(path string) (*os.File, error) { return os.Create(path) }

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestWithApprovalShimSkipsBypassingLanes(t *testing.T) {
	added := withApprovalShim([]string{"--permission-mode", "acceptEdits"}, "/bin/runner", "/tmp/x-approve")
	if len(added) != 6 || added[2] != "--permission-prompt-tool" || added[3] != approvalPromptTool || added[4] != "--mcp-config" ||
		!strings.Contains(added[5], `"approval-shim","--socket","/tmp/x-approve"`) {
		t.Fatalf("shim args = %#v", added)
	}
	for _, args := range [][]string{
		{"--dangerously-skip-permissions"},
		{"--permission-mode", "bypassPermissions"},
		{"--permission-prompt-tool", "mcp__other__ask"},
	} {
		if got := withApprovalShim(args, "/bin/runner", "/tmp/x-approve"); len(got) != len(args) {
			t.Fatalf("shim added to %v: %v", args, got)
		}
	}
}

func TestApprovalShimWindowsPathsRoundTripThroughMCPConfig(t *testing.T) {
	runner := `C:\Program Files\Sessions\runtime\sessions-runner.exe`
	socket := `\\.\pipe\somewhere-sessions-0123456789ab-11111111-2222-4333-8444-555555555555`
	endpoint := approvalEndpoint(socket)
	if endpoint != socket+"-approve" {
		t.Fatalf("approval endpoint = %q", endpoint)
	}

	args := withApprovalShim(nil, runner, endpoint)
	if len(args) != 4 || args[2] != "--mcp-config" {
		t.Fatalf("Claude args = %#v", args)
	}
	var definition struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[3]), &definition); err != nil {
		t.Fatalf("decode MCP config %q: %v", args[3], err)
	}
	server := definition.MCPServers["sessions"]
	if server.Command != runner {
		t.Fatalf("shim executable = %q, want %q", server.Command, runner)
	}
	if got := approvalShimEndpointArg(server.Args); got != endpoint {
		t.Fatalf("shim --socket = %q, want %q; args=%#v", got, endpoint, server.Args)
	}
}

// The shim speaks MCP on stdio and forwards each tools/call to the runner's
// approval endpoint, returning Claude's allow/deny payload.
func TestApprovalShimForwardsToTheRunner(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan shimApprovalRequest, 2)
	decisions := []shimApprovalReply{{Decision: proto.ApprovalAllow}, {Decision: proto.ApprovalDeny, By: "manager-1"}}
	go serveApprovalSocket(listener, func(request shimApprovalRequest) shimApprovalReply {
		seen <- request
		next := decisions[0]
		decisions = decisions[1:]
		return next
	})
	dial := func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", listener.Addr().String())
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- serveApprovalShim(stdinReader, stdoutWriter, dial) }()
	responses := bufio.NewReader(stdoutReader)
	send := func(line string) {
		if _, err := io.WriteString(stdinWriter, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	next := func() map[string]any {
		t.Helper()
		type read struct {
			line []byte
			err  error
		}
		got := make(chan read, 1)
		go func() {
			line, err := responses.ReadBytes('\n')
			got <- read{line, err}
		}()
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatal(r.err)
			}
			var value map[string]any
			if err := json.Unmarshal(r.line, &value); err != nil {
				t.Fatalf("decode %s: %v", r.line, err)
			}
			return value
		case <-time.After(5 * time.Second):
			t.Fatal("shim wrote nothing")
			return nil
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"x"}}}`)
	if init := next(); init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %#v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := next()["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != approvalShimToolName {
		t.Fatalf("tools = %#v", tools)
	}

	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"npm test"},"tool_use_id":"toolu_1"}}}`)
	request := <-seen
	if request.ToolName != "Bash" || request.ToolUseID != "toolu_1" || !strings.Contains(string(request.Input), "npm test") {
		t.Fatalf("forwarded request = %#v", request)
	}
	allowed := next()
	text := allowed["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if json.Unmarshal([]byte(text), &payload) != nil || payload["behavior"] != "allow" || payload["updatedInput"].(map[string]any)["command"] != "npm test" {
		t.Fatalf("allow payload = %s", text)
	}

	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Edit","input":{"file_path":"a.go"}}}}`)
	<-seen
	denied := next()
	text = denied["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if json.Unmarshal([]byte(text), &payload) != nil || payload["behavior"] != "deny" || !strings.Contains(payload["message"].(string), "delegated") {
		t.Fatalf("deny payload = %s", text)
	}

	send(`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	if unsupported := next(); unsupported["error"] == nil {
		t.Fatalf("unsupported method answered: %#v", unsupported)
	}
	_ = stdinWriter.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("shim exit = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shim did not exit on stdin close")
	}
}

func TestClaudeRunnerHoldsAnApprovalUntilTheDaemonAnswers(t *testing.T) {
	paths := stateFor(t)
	file, err := osCreate(paths.Structured)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	r := &claudeStructuredRunner{
		cfg: config{id: paths.ID, cmd: "claude", cwd: "/tmp"}, paths: paths, sessionID: "s-1",
		logger: discardLogger(), clients: make(map[*client]struct{}), historyFile: file, ctx: context.Background(),
	}
	replies := make(chan shimApprovalReply, 1)
	go func() {
		replies <- r.decideApproval(shimApprovalRequest{ToolName: "Bash", ToolUseID: "toolu_1", Input: json.RawMessage(`{"command":"touch a.txt"}`)})
	}()
	var id string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && id == ""; {
		r.mu.Lock()
		for _, raw := range r.history {
			var value struct {
				Subtype  string `json:"subtype"`
				Approval struct {
					ID      string `json:"id"`
					Summary string `json:"summary"`
				} `json:"approval"`
			}
			if json.Unmarshal(raw, &value) == nil && value.Subtype == "approval_requested" {
				if value.Approval.Summary != "Run `touch a.txt`" {
					t.Errorf("summary = %q", value.Approval.Summary)
				}
				id = value.Approval.ID
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("runner never announced the approval")
	}
	select {
	case reply := <-replies:
		t.Fatalf("runner answered %#v without a decision", reply)
	case <-time.After(100 * time.Millisecond):
	}
	payload, err := proto.EncodeApprovalControl(proto.ApprovalControl{ID: id, Decision: proto.ApprovalAllowForSession, By: "manager-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.handleFrame(nil, proto.Frame{Type: proto.Approve, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-replies:
		if reply.Decision != proto.ApprovalAllowForSession || reply.By != "manager-1" {
			t.Fatalf("reply = %#v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the waiting approval")
	}
}
